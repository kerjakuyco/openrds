package main

// Servers: the machines databases run on. The console's own server is
// "local". Others are Docker hosts reached over SSH with a key the console
// generates, their host key pinned when they are added. Every Docker and
// Compose call for a database runs against its server, so backups, upgrades,
// users and everything else work the same on each one. Databases keep running
// when the console is down; only management pauses.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	localServer = "local"
	// A server that just failed is not tried again for this long, so a
	// server that is down does not slow every page.
	downRetry = 30 * time.Second
	// The oldest Docker Engine a server may run.
	minDockerMajor = 28
)

var (
	serverIDPattern   = regexp.MustCompile(`^(local|s[0-9a-f]{8})$`)
	sshUserPattern    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	serverNamePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()-]{0,39}$`)
	hostKeyTypes      = []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa"}
	// Private access forwards only these sources unless you choose others.
	privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}
)

type server struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// SSH: user@host or user@host:port. Empty for the console's own server.
	Address string `json:"address,omitempty"`
	// Where other servers reach this one: private access listens here.
	PrivateAddress string `json:"privateAddress,omitempty"`
	// Where clients on the internet reach this one, for public access.
	PublicAddress string    `json:"publicAddress,omitempty"`
	Added         time.Time `json:"addedAt,omitzero"`
}

func sameServer(a, b string) bool {
	if a == "" {
		a = localServer
	}
	if b == "" {
		b = localServer
	}
	return a == b
}

type sshTarget struct {
	user, host string
	port       int
}

// Parse user@host or user@host:port.
func parseSSH(address string) (sshTarget, error) {
	bad := fail(400, "Use user@host or user@host:port for the SSH address, for example openrds@10.0.0.5.")
	user, hostport, ok := strings.Cut(strings.TrimSpace(address), "@")
	if !ok || !sshUserPattern.MatchString(user) {
		return sshTarget{}, bad
	}
	t := sshTarget{user: user, host: hostport, port: 22}
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return sshTarget{}, bad
		}
		t.host, t.port = h, port
	}
	if net.ParseIP(t.host) == nil && (len(t.host) > 253 || !hostPattern.MatchString(t.host)) {
		return sshTarget{}, bad
	}
	return t, nil
}

func (t sshTarget) url() string {
	return "ssh://" + t.user + "@" + net.JoinHostPort(t.host, strconv.Itoa(t.port))
}

// The host as known_hosts names it.
func (t sshTarget) knownHost() string {
	if t.port == 22 {
		return t.host
	}
	return "[" + t.host + "]:" + strconv.Itoa(t.port)
}

// ---------- the registry

type serverHealth struct {
	reachable bool
	checked   time.Time
	problem   string
	alerted   bool // the console reported it unreachable
}

type serverState struct {
	mu      sync.Mutex
	loaded  bool
	list    map[string]server
	health  map[string]serverHealth
	info    map[string]serverInfo
	infoAt  map[string]time.Time // when info was read
	engines map[string]statsReader
	sshOnce sync.Once
	sshErr  error
}

func (m *manager) serversDir() string { return filepath.Join(m.dir, "servers") }
func (m *manager) sshDir() string     { return filepath.Join(m.dir, "ssh") }

func (m *manager) loadServers() {
	if m.servers.loaded {
		return
	}
	m.servers.list = map[string]server{}
	entries, _ := os.ReadDir(m.serversDir())
	for _, e := range entries {
		var s server
		if strings.HasSuffix(e.Name(), ".json") && readJSON(filepath.Join(m.serversDir(), e.Name()), &s) == nil && serverIDPattern.MatchString(s.ID) {
			m.servers.list[s.ID] = s
		}
	}
	m.servers.loaded = true
}

// Every server, the console's own first.
func (m *manager) serverList() []server {
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	m.loadServers()
	local, ok := m.servers.list[localServer]
	if !ok {
		local = server{ID: localServer}
	}
	if local.Name == "" {
		local.Name = "This server"
	}
	out := []server{local}
	for _, id := range slices.Sorted(maps.Keys(m.servers.list)) {
		if id != localServer {
			out = append(out, m.servers.list[id])
		}
	}
	return out
}

func (m *manager) server(id string) (server, bool) {
	if id == "" {
		id = localServer
	}
	for _, s := range m.serverList() {
		if s.ID == id {
			return s, true
		}
	}
	return server{}, false
}

func (m *manager) saveServer(s server) error {
	if err := os.MkdirAll(m.serversDir(), 0700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(m.serversDir(), s.ID+".json"), s); err != nil {
		return err
	}
	m.servers.mu.Lock()
	m.loadServers()
	m.servers.list[s.ID] = s
	delete(m.servers.engines, s.ID)
	m.servers.mu.Unlock()
	return nil
}

// ---------- Docker on a server

// Runs Docker against one server: the environment points the CLI at it.
type serverRunner struct {
	base dockerRunner
	name string
	env  []string
}

func (s serverRunner) run(ctx context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
	err := s.base.run(ctx, append(slices.Clone(s.env), env...), in, out, args...)
	if err != nil && (strings.Contains(err.Error(), "error during connect") || strings.Contains(err.Error(), "ssh: ") ||
		strings.Contains(err.Error(), "Host key verification failed") || strings.Contains(err.Error(), "Permission denied (publickey")) {
		return fmt.Errorf("cannot reach server %s over SSH: %w", s.name, err)
	}
	return err
}

type failingRunner struct{ err error }

func (f failingRunner) run(context.Context, []string, io.Reader, io.Writer, ...string) error {
	return f.err
}

// Docker on a server: the console's own, or another one over SSH.
func (m *manager) dockerFor(id string) dockerRunner {
	if id == "" || id == localServer {
		return m.docker
	}
	s, ok := m.server(id)
	if !ok {
		return failingRunner{fmt.Errorf("server %s is not registered", id)}
	}
	env, err := m.serverEnv(s)
	if err != nil {
		return failingRunner{err}
	}
	return serverRunner{m.docker, s.Name, env}
}

// Docker on the database's server.
func (m *manager) on(r record) dockerRunner { return m.dockerFor(r.Server) }

func (m *manager) serverEnv(s server) ([]string, error) {
	t, err := parseSSH(s.Address)
	if err != nil {
		return nil, err
	}
	if err := m.prepareSSH(); err != nil {
		return nil, err
	}
	return []string{"DOCKER_HOST=" + t.url(), "PATH=" + filepath.Join(m.sshDir(), "bin") + string(os.PathListSeparator) + os.Getenv("PATH")}, nil
}

// The SSH settings Docker uses: the console's key, only pinned host keys,
// and one shared connection per server, kept open a while so the many
// short Docker calls don't each pay for a handshake. Docker runs "ssh" from
// PATH, so a wrapper in front of it adds them.
func (m *manager) prepareSSH() error {
	m.servers.sshOnce.Do(func() {
		dir := m.sshDir()
		ssh, err := exec.LookPath("ssh")
		if err != nil {
			ssh = "ssh"
		}
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
		config := fmt.Sprintf(`# Written by OpenRDS for Docker over SSH.
Host *
  IdentityFile "%s"
  IdentitiesOnly yes
  UserKnownHostsFile "%s"
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  HashKnownHosts no
  BatchMode yes
  ConnectTimeout 10
  ServerAliveInterval 15
  ServerAliveCountMax 3
  ControlMaster auto
  ControlPath /tmp/openrds-ssh-%%C
  ControlPersist 5m
  LogLevel ERROR
`, filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "known_hosts"))
		wrapper := fmt.Sprintf("#!/bin/sh\nexec %s -F %s \"$@\"\n", quote(ssh), quote(filepath.Join(dir, "config")))
		err = os.MkdirAll(filepath.Join(dir, "bin"), 0700)
		if err == nil {
			err = writeFile(filepath.Join(dir, "config"), []byte(config))
		}
		if err == nil {
			err = writeFile(filepath.Join(dir, "bin", "ssh"), []byte(wrapper))
		}
		if err == nil {
			err = os.Chmod(filepath.Join(dir, "bin", "ssh"), 0700)
		}
		if err == nil {
			f, openErr := os.OpenFile(filepath.Join(dir, "known_hosts"), os.O_CREATE|os.O_WRONLY, 0600)
			if err = openErr; err == nil {
				err = f.Close()
			}
		}
		m.servers.sshErr = err
	})
	return m.servers.sshErr
}

// The console's public key, created the first time it is needed.
func (m *manager) publicKey(ctx context.Context) (string, error) {
	if err := m.prepareSSH(); err != nil {
		return "", err
	}
	path := filepath.Join(m.sshDir(), "id_ed25519")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "openrds", "-f", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("creating the SSH key: %v %s", err, bytes.TrimSpace(out))
		}
	}
	key, err := os.ReadFile(path + ".pub")
	return strings.TrimSpace(string(key)), err
}

// ---------- host keys

type hostKeys struct {
	Keys         []string `json:"hostKeys"`
	Fingerprints []string `json:"fingerprints"`
}

// The server's host keys and their fingerprints, to compare with the ones
// the server shows (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub).
func scanHostKeys(ctx context.Context, t sshTarget) (hostKeys, error) {
	var keys hostKeys
	scan := exec.CommandContext(ctx, "ssh-keyscan", "-T", "5", "-p", strconv.Itoa(t.port), "--", t.host)
	out, err := scan.Output()
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") && validHostKey(t, line) {
			keys.Keys = append(keys.Keys, line)
		}
	}
	if len(keys.Keys) == 0 {
		if err == nil {
			err = errors.New("no host keys")
		}
		return keys, fail(502, fmt.Sprintf("Could not read the SSH host keys of %s on port %d: %v. Check the address and that SSH is reachable from the console.", t.host, t.port, err))
	}
	print := exec.CommandContext(ctx, "ssh-keygen", "-l", "-f", "-")
	print.Stdin = strings.NewReader(strings.Join(keys.Keys, "\n") + "\n")
	lines, err := print.Output()
	if err != nil {
		return keys, fmt.Errorf("reading host key fingerprints: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(lines)), "\n") {
		f := strings.Fields(line) // bits fingerprint host (type)
		if len(f) >= 4 {
			keys.Fingerprints = append(keys.Fingerprints, strings.Trim(f[len(f)-1], "()")+" "+f[1])
		}
	}
	return keys, nil
}

// A known_hosts line for exactly this server, without markers or patterns.
func validHostKey(t sshTarget, line string) bool {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != t.knownHost() || !slices.Contains(hostKeyTypes, f[1]) {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(f[2])
	return err == nil
}

// Replace the pinned keys of one host.
func (m *manager) pinHostKeys(t sshTarget, keys []string) error {
	path := filepath.Join(m.sshDir(), "known_hosts")
	current, _ := os.ReadFile(path)
	var lines []string
	for _, line := range strings.Split(string(current), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] != t.knownHost() {
			lines = append(lines, line)
		}
	}
	lines = append(lines, keys...)
	data := strings.Join(lines, "\n")
	if data != "" {
		data += "\n"
	}
	return writeFile(path, []byte(data))
}

// ---------- adding, changing and removing servers

type serverRequest struct {
	Name           string   `json:"name"`
	Address        string   `json:"address"`
	HostKeys       []string `json:"hostKeys"`
	PrivateAddress string   `json:"privateAddress"`
	PublicAddress  string   `json:"publicAddress"`
}

func checkServerNames(s server, others []server) error {
	if !serverNamePattern.MatchString(s.Name) {
		return fail(400, "Give the server a name of 1–40 letters, digits, spaces and . _ ( ) -.")
	}
	if s.PrivateAddress != "" {
		a, err := netip.ParseAddr(s.PrivateAddress)
		if err != nil || !a.Is4() {
			return fail(400, "Use the server’s private IPv4 address, such as 10.0.0.5.")
		}
	}
	if p := s.PublicAddress; p != "" && net.ParseIP(p) == nil && (len(p) > 253 || !hostPattern.MatchString(p)) {
		return fail(400, "Use an IP address or a DNS name for the public address.")
	}
	for _, o := range others {
		if o.ID == s.ID {
			continue
		}
		if strings.EqualFold(o.Name, s.Name) {
			return fail(409, "Another server has that name.")
		}
		if s.Address != "" && o.Address == s.Address {
			return fail(409, o.Name+" already uses that SSH address.")
		}
	}
	return nil
}

// Connect to a new server over SSH with the host keys the admin confirmed,
// check its Docker Engine, copy the helper images and register it.
func (m *manager) addServer(ctx context.Context, req serverRequest) (s server, err error) {
	t, err := parseSSH(req.Address)
	if err != nil {
		return s, err
	}
	s = server{ID: "s" + randomHex(4), Name: strings.TrimSpace(req.Name), Address: req.Address, Added: time.Now().UTC(),
		PrivateAddress: strings.TrimSpace(req.PrivateAddress), PublicAddress: strings.TrimSpace(req.PublicAddress)}
	if err := checkServerNames(s, m.serverList()); err != nil {
		return s, err
	}
	if len(req.HostKeys) == 0 || slices.ContainsFunc(req.HostKeys, func(k string) bool { return !validHostKey(t, k) }) {
		return s, fail(400, "Confirm the server’s host keys first.")
	}
	if _, err := m.publicKey(ctx); err != nil {
		return s, err
	}
	finish := m.trackServer(ctx, s.Name, "Add server")
	defer func() { finish(err) }()
	if err := m.pinHostKeys(t, req.HostKeys); err != nil {
		return s, err
	}
	env, err := m.serverEnv(s)
	if err != nil {
		return s, err
	}
	runner := serverRunner{m.docker, s.Name, env}
	check, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := runner.run(check, nil, nil, &out, "version", "--format", "{{.Server.Version}}"); err != nil {
		m.pinHostKeys(t, nil)
		return s, fail(502, "Could not use Docker on "+t.host+" over SSH. Check that the console’s key is in "+t.user+"’s authorized_keys and that "+t.user+" is in the docker group. "+describe(err))
	}
	version := strings.TrimSpace(out.String())
	if major, _, _ := strings.Cut(version, "."); atoi(major) < minDockerMajor {
		m.pinHostKeys(t, nil)
		return s, fail(422, fmt.Sprintf("%s runs Docker Engine %s. OpenRDS needs %d or newer.", t.host, version, minDockerMajor))
	}
	if err := m.saveServer(s); err != nil {
		return s, err
	}
	m.noteReachable(s.ID, nil)
	if err := m.syncImages(ctx, s.ID); err != nil {
		log.Printf("copying helper images to %s: %v", s.Name, err)
	}
	// Addresses nobody gave are found, here and on the console's server, so
	// private access and replicas between them work without looking them up.
	if filled := m.fillAddresses(ctx, s); filled != s {
		if err := m.saveServer(filled); err == nil {
			s = filled
		}
	}
	if local, _ := m.server(localServer); local.PrivateAddress == "" {
		if filled := m.fillAddresses(ctx, local); filled.PrivateAddress != "" {
			m.saveServer(filled)
		}
	}
	return s, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (m *manager) updateServer(ctx context.Context, id string, req serverRequest) (server, error) {
	s, ok := m.server(id)
	if !ok {
		return s, fail(404, "Server not found.")
	}
	s.Name, s.PrivateAddress, s.PublicAddress = strings.TrimSpace(req.Name), strings.TrimSpace(req.PrivateAddress), strings.TrimSpace(req.PublicAddress)
	if s.ID == localServer && s.Name == "" {
		s.Name = "This server"
	}
	if err := checkServerNames(s, m.serverList()); err != nil {
		return s, err
	}
	all, err := m.records()
	if err != nil {
		return s, err
	}
	if s.PrivateAddress == "" {
		for _, r := range all {
			if sameServer(r.Server, id) && r.PrivateAccess {
				return s, fail(409, r.Name+" uses private access on this server. Turn it off first.")
			}
		}
	}
	old, _ := m.server(id)
	for _, rep := range all {
		if rep.SourceRoute == "" {
			continue
		}
		var src record
		for _, r := range all {
			if r.Name == rep.ReplicaOf {
				src = r
			}
		}
		uses := rep.SourceRoute == routePrivate && s.PrivateAddress != old.PrivateAddress && (sameServer(rep.Server, id) || sameServer(src.Server, id)) ||
			rep.SourceRoute == routePublic && s.PublicAddress != old.PublicAddress && (sameServer(rep.Server, id) || sameServer(src.Server, id) && src.PublicHost == "")
		if uses {
			return s, fail(409, fmt.Sprintf("Read replica %s replicates from %s over this address. Promote or delete it before changing the address.", rep.Name, rep.ReplicaOf))
		}
	}
	if err := m.saveServer(s); err != nil {
		return s, err
	}
	m.noteServer(ctx, s.Name, "Change server", "")
	return s, nil
}

func (m *manager) removeServer(ctx context.Context, id string) error {
	s, ok := m.server(id)
	if !ok {
		return fail(404, "Server not found.")
	}
	if id == localServer {
		return fail(409, "The console’s own server can’t be removed.")
	}
	all, err := m.records()
	if err != nil {
		return err
	}
	var names []string
	for _, r := range all {
		if sameServer(r.Server, id) {
			names = append(names, r.Name)
		}
	}
	if len(names) > 0 {
		return fail(409, fmt.Sprintf("%s still runs %s. Move or delete them first.", s.Name, strings.Join(names, ", ")))
	}
	if err := os.Remove(filepath.Join(m.serversDir(), id+".json")); err != nil {
		return err
	}
	m.servers.mu.Lock()
	delete(m.servers.list, id)
	delete(m.servers.health, id)
	delete(m.servers.info, id)
	delete(m.servers.infoAt, id)
	delete(m.servers.engines, id)
	m.servers.mu.Unlock()
	if t, err := parseSSH(s.Address); err == nil {
		m.pinHostKeys(t, nil)
	}
	m.noteServer(ctx, s.Name, "Remove server", "")
	return nil
}

// ---------- finding a server's addresses

// A private IPv4 address, in the ranges private access allows by default.
func privateIPv4(text string) bool {
	a, err := netip.ParseAddr(text)
	if err != nil || !a.Is4() {
		return false
	}
	for _, r := range privateRanges {
		if netip.MustParsePrefix(r).Contains(a) {
			return true
		}
	}
	return false
}

// The private IPv4 addresses a server's own interfaces have, read with the
// disk helper image on the host network, without Docker's own bridges.
func (m *manager) detectPrivate(ctx context.Context, id string) []string {
	var out bytes.Buffer
	if m.dockerFor(id).run(ctx, nil, nil, &out, "run", "--rm", "--network", "host", "--entrypoint", "ip", diskHelperImage(), "-4", "-o", "addr", "show") != nil {
		return nil
	}
	var found []string
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line) // 2: eth0 inet 10.0.0.5/24 brd ...
		if len(f) < 4 || f[2] != "inet" {
			continue
		}
		ip, _, _ := strings.Cut(f[3], "/")
		name := f[1]
		if name == "lo" || name == "docker0" || strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "veth") || !privateIPv4(ip) {
			continue
		}
		found = append(found, ip)
	}
	return found
}

// Fill in the addresses a server lacks: the SSH host when it is a private
// address, otherwise one its interfaces have; and the SSH host as its public
// address when that is not private.
func (m *manager) fillAddresses(ctx context.Context, s server) server {
	host := ""
	if t, err := parseSSH(s.Address); err == nil {
		host = t.host
	}
	if s.PrivateAddress == "" {
		if privateIPv4(host) {
			s.PrivateAddress = host
		} else if found := m.detectPrivate(ctx, s.ID); len(found) > 0 {
			s.PrivateAddress = found[0]
		}
	}
	if a, err := netip.ParseAddr(host); s.PublicAddress == "" && host != "" && !privateIPv4(host) && (err != nil || !a.IsLoopback()) {
		s.PublicAddress = host
	}
	return s
}

// ---------- helper images

// The images the console builds itself, which a server can't pull: the
// gateway (the console's own image), the connection pooler and the disk
// helper. Each is copied when the server lacks it or has another build.
func (m *manager) syncImages(ctx context.Context, id string) error {
	images := []string{diskHelperImage(), env("OPENRDS_POOLER_IMAGE", "openrds-pooler:local")}
	if gateway, err := m.gatewayImage(ctx); err == nil {
		images = append(images, gateway)
	}
	local, remote := m.docker, m.dockerFor(id)
	var missing []string
	for _, image := range images {
		var have, want bytes.Buffer
		if local.run(ctx, nil, nil, &want, "image", "inspect", "--format", "{{.Id}}", image) != nil {
			continue // not built here, so not offered there either
		}
		remote.run(ctx, nil, nil, &have, "image", "inspect", "--format", "{{.Id}}", image)
		if strings.TrimSpace(have.String()) != strings.TrimSpace(want.String()) {
			missing = append(missing, image)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var localArch, remoteArch bytes.Buffer
	local.run(ctx, nil, nil, &localArch, "info", "--format", "{{.Architecture}}")
	if err := remote.run(ctx, nil, nil, &remoteArch, "info", "--format", "{{.Architecture}}"); err != nil {
		return err
	}
	if strings.TrimSpace(localArch.String()) != strings.TrimSpace(remoteArch.String()) {
		return fmt.Errorf("the server is %s and the console %s, so public and private access, pooling and disk limits need images built there",
			strings.TrimSpace(remoteArch.String()), strings.TrimSpace(localArch.String()))
	}
	pr, pw := io.Pipe()
	saved := make(chan error, 1)
	go func() {
		err := local.run(ctx, nil, nil, pw, append([]string{"save"}, missing...)...)
		pw.CloseWithError(err)
		saved <- err
	}()
	err := remote.run(ctx, nil, pr, io.Discard, "load", "--quiet")
	pr.CloseWithError(errors.New("the copy ended"))
	if saveErr := <-saved; saveErr != nil && err == nil {
		err = saveErr
	}
	if err == nil {
		m.quotaCache.forget(id)
	}
	return err
}

// Bring every other server's helper images up to date, at startup.
func (m *manager) syncAllImages(ctx context.Context) {
	for _, s := range m.serverList()[1:] {
		if err := m.syncImages(ctx, s.ID); err != nil {
			log.Printf("copying helper images to %s: %v", s.Name, err)
		}
	}
}

// ---------- reachability

func (m *manager) noteReachable(id string, err error) {
	if id == "" || id == localServer {
		return
	}
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	if m.servers.health == nil {
		m.servers.health = make(map[string]serverHealth)
	}
	h := m.servers.health[id]
	h.reachable, h.checked, h.problem = err == nil, time.Now(), ""
	if err != nil {
		h.problem = strings.TrimPrefix(err.Error(), "docker: ")
	}
	m.servers.health[id] = h
}

// Whether a server failed so recently that it is not tried again yet.
func (m *manager) recentlyDown(id string) bool {
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	h, ok := m.servers.health[id]
	return ok && !h.reachable && time.Since(h.checked) < downRetry
}

func (m *manager) unreachable() map[string]bool {
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	down := map[string]bool{}
	for id, h := range m.servers.health {
		if !h.reachable {
			down[id] = true
		}
	}
	return down
}

// Check every other server once a minute. Losing one, and getting it back,
// is an alert: its databases may still run, but the console can't manage,
// back up or monitor them meanwhile.
func (m *manager) watchServers(ctx context.Context) {
	for _, s := range m.serverList()[1:] {
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := m.dockerInfo(check, s.ID)
		cancel()
		m.noteReachable(s.ID, err)
		m.servers.mu.Lock()
		h := m.servers.health[s.ID]
		raise, recovered := !h.reachable && !h.alerted, h.reachable && h.alerted
		h.alerted = !h.reachable
		m.servers.health[s.ID] = h
		m.servers.mu.Unlock()
		e := event{Name: s.Name, Kind: "server"}
		switch {
		case raise:
			e.Action, e.Result = "Server unreachable", "failed"
			e.Detail = "OpenRDS can’t reach it over SSH, so it can’t manage, back up or monitor its databases. They may still be running. " + h.problem
			m.raise(ctx, e, alert{Category: categoryHealth, Severity: "critical", Title: s.Name + " is unreachable", Detail: e.Detail})
		case recovered:
			e.Action, e.Result = "Server reachable again", "completed"
			m.raise(ctx, e, alert{Category: categoryHealth, Severity: "resolved", Title: s.Name + " is reachable again"})
			go m.syncImages(context.WithoutCancel(ctx), s.ID)
		}
	}
}

// ---------- capacity

type serverInfo struct {
	Name        string `json:"hostname"`
	CPUs        int    `json:"cpus"`
	MemoryBytes int64  `json:"memoryBytes"`
	Version     string `json:"dockerVersion"`
	Arch        string `json:"architecture"`
}

const infoFormat = `{"hostname":{{json .Name}},"cpus":{{.NCPU}},"memoryBytes":{{.MemTotal}},"dockerVersion":{{json .ServerVersion}},"architecture":{{json .Architecture}}}`

// A server's capacity is read again after this long. Whether another
// server is reachable is known sooner: every refresh lists its containers.
const infoFresh = 30 * time.Second

type infoReader interface {
	info(ctx context.Context) (serverInfo, error)
}

// What docker info says about a server. The console's own Docker answers
// through the Engine API: the docker CLI starts every CLI plugin to answer
// docker info, a third of a CPU second each time. Other servers answer
// through the CLI, whose errors say why a server can't be reached.
func (m *manager) dockerInfo(ctx context.Context, id string) (serverInfo, error) {
	var info serverInfo
	if api, ok := m.engine.(infoReader); ok && (id == "" || id == localServer) {
		var err error
		if info, err = api.info(ctx); err != nil {
			return info, err
		}
	} else {
		var out bytes.Buffer
		if err := m.dockerFor(id).run(ctx, nil, nil, &out, "info", "--format", infoFormat); err != nil {
			return info, err
		}
		if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &info); err != nil {
			return info, fmt.Errorf("reading docker info: %w", err)
		}
	}
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	if m.servers.info == nil {
		m.servers.info, m.servers.infoAt = make(map[string]serverInfo), make(map[string]time.Time)
	}
	m.servers.info[id], m.servers.infoAt[id] = info, time.Now()
	return info, nil
}

// A server's capacity read within infoFresh, while it is reachable.
func (m *manager) freshInfo(id string) (serverInfo, bool) {
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	info, ok := m.servers.info[id]
	if !ok || time.Since(m.servers.infoAt[id]) >= infoFresh {
		return info, false
	}
	if id == "" || id == localServer {
		return info, true
	}
	h := m.servers.health[id]
	return info, h.reachable && time.Since(h.checked) < infoFresh
}

type serverView struct {
	server
	Local     bool         `json:"local"`
	Reachable bool         `json:"reachable"`
	Problem   string       `json:"problem,omitempty"`
	Info      *serverInfo  `json:"info,omitempty"`
	Databases []string     `json:"databases"`
	Allocated int          `json:"allocatedMemoryMiB"` // memory limits of its databases
	Quota     quotaSupport `json:"volumeQuota"`
	// Where a new database goes unless you choose: the reachable server with
	// the most memory not yet given to databases.
	Suggested bool `json:"suggested"`
}

func (m *manager) serverViews(ctx context.Context) ([]serverView, error) {
	all, err := m.records()
	if err != nil {
		return nil, err
	}
	list := m.serverList()
	views := make([]serverView, len(list))
	var wg sync.WaitGroup
	for i, s := range list {
		v := serverView{server: s, Local: s.ID == localServer, Databases: []string{}}
		for _, r := range all {
			if sameServer(r.Server, s.ID) {
				v.Databases = append(v.Databases, r.Name)
				v.Allocated += r.MemoryMiB
			}
		}
		views[i] = v
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i].fill(ctx, m)
		}()
	}
	wg.Wait()
	best, room := 0, int64(-1)
	for i, v := range views {
		if v.Reachable && v.Info != nil {
			if free := v.Info.MemoryBytes>>20 - int64(v.Allocated); free > room {
				best, room = i, free
			}
		}
	}
	views[best].Suggested = true
	return views, nil
}

func (v *serverView) fill(ctx context.Context, m *manager) {
	if !v.Local && m.recentlyDown(v.ID) {
		m.servers.mu.Lock()
		v.Problem = m.servers.health[v.ID].problem
		m.servers.mu.Unlock()
		return
	}
	info, ok := m.freshInfo(v.ID)
	if !ok {
		check, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var err error
		info, err = m.dockerInfo(check, v.ID)
		if ctx.Err() != nil {
			v.Reachable = !m.recentlyDown(v.ID) // the page went away: no news about the server
			return
		}
		m.noteReachable(v.ID, err)
		if err != nil {
			v.Problem = strings.TrimPrefix(err.Error(), "docker: ")
			return
		}
	}
	v.Info = &info
	v.Reachable = true
	v.Quota = m.volumeQuota(ctx, v.ID)
}

// ---------- metrics over SSH

// The Docker Engine API of another server, reached through "docker system
// dial-stdio" over the same SSH connection Docker uses.
func (m *manager) statsFor(id string) statsReader {
	if id == "" || id == localServer {
		return m.engine
	}
	m.servers.mu.Lock()
	e, ok := m.servers.engines[id]
	m.servers.mu.Unlock()
	if ok {
		return e
	}
	s, ok := m.server(id)
	if !ok {
		return nil
	}
	env, err := m.serverEnv(s)
	if err != nil {
		return nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) { return dialStdio(env) }
	transport := &http.Transport{DialContext: dial, MaxIdleConns: 2, IdleConnTimeout: time.Minute}
	e = engineAPI{&http.Client{Timeout: 20 * time.Second, Transport: transport}}
	m.servers.mu.Lock()
	defer m.servers.mu.Unlock()
	if m.servers.engines == nil {
		m.servers.engines = make(map[string]statsReader)
	}
	m.servers.engines[id] = e
	return e
}

type stdioConn struct {
	cmd *exec.Cmd
	io.Reader
	io.WriteCloser
}

func dialStdio(env []string) (net.Conn, error) {
	cmd := exec.Command("docker", "system", "dial-stdio")
	cmd.Env = append(os.Environ(), env...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &stdioConn{cmd, bufio.NewReader(out), in}, nil
}

func (c *stdioConn) Close() error {
	c.WriteCloser.Close()
	c.cmd.Process.Kill()
	c.cmd.Wait()
	return nil
}

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "docker" }

func (c *stdioConn) LocalAddr() net.Addr              { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr             { return stdioAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

// ---------- API

func serverRoutes(add func(string, int, endpoint), m *manager) {
	add("GET /api/servers", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		views, err := m.serverViews(r.Context())
		if err == nil {
			if i, _ := identityFrom(r.Context()); i.level() < roleAdmin {
				for i := range views {
					views[i].Address = "" // who can log in where is for admins
				}
			}
			sendJSON(w, 200, views)
		}
		return err
	})
	add("GET /api/servers/key", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		key, err := m.publicKey(r.Context())
		if err == nil {
			sendJSON(w, 200, map[string]string{"publicKey": key, "setup": setupCommand(key)})
		}
		return err
	})
	add("POST /api/servers/scan", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Address string `json:"address"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		t, err := parseSSH(body.Address)
		if err != nil {
			return err
		}
		keys, err := scanHostKeys(r.Context(), t)
		if err == nil {
			sendJSON(w, 200, keys)
		}
		return err
	})
	add("POST /api/servers", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var req serverRequest
		if err := decode(w, r, &req); err != nil {
			return err
		}
		s, err := m.addServer(r.Context(), req)
		if err == nil {
			sendJSON(w, 201, s)
		}
		return err
	})
	add("POST /api/servers/{id}/detect", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		s, ok := m.server(r.PathValue("id"))
		if !ok {
			return fail(404, "Server not found.")
		}
		found := m.detectPrivate(r.Context(), s.ID)
		suggested := m.fillAddresses(r.Context(), server{ID: s.ID, Address: s.Address})
		sendJSON(w, 200, map[string]any{"private": found, "privateAddress": suggested.PrivateAddress, "publicAddress": suggested.PublicAddress})
		return nil
	})
	add("PUT /api/servers/{id}", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var req serverRequest
		if err := decode(w, r, &req); err != nil {
			return err
		}
		s, err := m.updateServer(r.Context(), r.PathValue("id"), req)
		if err == nil {
			sendJSON(w, 200, s)
		}
		return err
	})
	add("DELETE /api/servers/{id}", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.removeServer(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}

// Server events are recorded under the server's name.
func (m *manager) noteServer(ctx context.Context, name, action, detail string) {
	m.logEvent(ctx, event{Name: name, Kind: "server", Action: action, Result: "completed", Detail: detail})
}

func (m *manager) trackServer(ctx context.Context, name, action string) func(error) {
	m.logEvent(ctx, event{Name: name, Kind: "server", Action: action, Result: "started"})
	return func(err error) {
		e := event{Name: name, Kind: "server", Action: action, Result: "completed"}
		if err != nil {
			e.Result, e.Detail = "failed", describe(err)
		}
		m.logEvent(ctx, e)
	}
}

// What to run on a new server so the console can use its Docker: a user in
// the docker group that accepts the console's key.
func setupCommand(key string) string {
	return fmt.Sprintf(`sudo sh -c 'id openrds >/dev/null 2>&1 || useradd --create-home --shell /bin/sh openrds
usermod -aG docker openrds
install -d -m 700 -o openrds -g openrds ~openrds/.ssh
echo "%s" >> ~openrds/.ssh/authorized_keys
chown openrds ~openrds/.ssh/authorized_keys && chmod 600 ~openrds/.ssh/authorized_keys'`, key)
}
