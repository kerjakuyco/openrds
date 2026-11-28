package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSSHAddressesAreParsed(t *testing.T) {
	for address, want := range map[string][2]string{
		"openrds@10.0.0.5":           {"ssh://openrds@10.0.0.5:22", "10.0.0.5"},
		"deploy@db.example.com:2222": {"ssh://deploy@db.example.com:2222", "[db.example.com]:2222"},
		"openrds@[fd00::5]:22":       {"ssh://openrds@[fd00::5]:22", "fd00::5"},
		" openrds@100.64.1.2 ":       {"ssh://openrds@100.64.1.2:22", "100.64.1.2"},
	} {
		tg, err := parseSSH(address)
		if err != nil || tg.url() != want[0] || tg.knownHost() != want[1] {
			t.Errorf("%q: %s %s %v", address, tg.url(), tg.knownHost(), err)
		}
	}
	for _, bad := range []string{"10.0.0.5", "root@", "Bad User@host", "openrds@host:0", "openrds@ho st", "openrds@host:22:33", "-oProxyCommand=x@host"} {
		if _, err := parseSSH(bad); status(err) != 400 {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestOnlyThatServersHostKeysArePinned(t *testing.T) {
	tg, _ := parseSSH("openrds@db.example.com:2222")
	good := "[db.example.com]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	if !validHostKey(tg, good) {
		t.Fatal("a scanned key of this server is pinned")
	}
	for _, bad := range []string{
		"@cert-authority * ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		"* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		"other.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		"[db.example.com]:2222 ssh-dss AAAAB3NzaC1kc3M=",
		"[db.example.com]:2222 ssh-ed25519 not*base64",
		good + " comment",
	} {
		if validHostKey(tg, bad) {
			t.Errorf("pinned %q", bad)
		}
	}
}

// Docker on several servers: each call answers for the server DOCKER_HOST names.
type fleet struct {
	mu    sync.Mutex
	calls map[string][][]string
	down  map[string]bool
	ps    map[string]string
}

func newFleet() *fleet {
	return &fleet{calls: map[string][][]string{}, down: map[string]bool{}, ps: map[string]string{}}
}

func dockerHostOf(env []string) string {
	for _, v := range env {
		if h, ok := strings.CutPrefix(v, "DOCKER_HOST="); ok {
			return h
		}
	}
	return "local"
}

func (f *fleet) run(_ context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := dockerHostOf(env)
	f.calls[host] = append(f.calls[host], args)
	if f.down[host] {
		return errors.New("docker: error during connect: ssh: connect to host 10.0.0.9 port 22: Connection refused")
	}
	if in != nil {
		io.Copy(io.Discard, in)
	}
	switch {
	case args[0] == "ps":
		fmt.Fprint(out, f.ps[host])
	case args[0] == "version":
		fmt.Fprint(out, "29.1.0\n")
	case args[0] == "info":
		fmt.Fprint(out, `{"hostname":"h","cpus":4,"memoryBytes":8589934592,"dockerVersion":"29.1.0","architecture":"x86_64"}`)
	}
	return nil
}

func (f *fleet) count(host string, verb string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.calls[host] {
		if a[0] == verb {
			n++
		}
	}
	return n
}

func addTestServer(t *testing.T, m *manager, id, address, private string) server {
	t.Helper()
	s := server{ID: id, Name: "db-" + id, Address: address, PrivateAddress: private, Added: time.Now()}
	if err := m.saveServer(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEveryDockerCallRunsOnTheDatabasesServer(t *testing.T) {
	f := newFleet()
	m := testManager(t, f.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "10.0.0.9")
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, Server: "s0000000a"}, Project: "openrds-db-orders"}
	if err := m.compose(t.Context(), r, nil, nil, "ps"); err != nil {
		t.Fatal(err)
	}
	if f.count("ssh://openrds@10.0.0.9:22", "compose") != 1 || f.count("local", "compose") != 0 {
		t.Fatalf("compose ran on: %v", f.calls)
	}
	env, _ := m.serverEnv(server{Address: "openrds@10.0.0.9"})
	if !slices.ContainsFunc(env, func(v string) bool { return strings.HasPrefix(v, "PATH="+filepath.Join(m.sshDir(), "bin")+":") }) {
		t.Fatalf("Docker finds the console's ssh wrapper first: %v", env)
	}
	config, _ := os.ReadFile(filepath.Join(m.sshDir(), "config"))
	for _, want := range []string{"StrictHostKeyChecking yes", "BatchMode yes", "UserKnownHostsFile \"" + filepath.Join(m.sshDir(), "known_hosts"), "ControlPersist", "IdentitiesOnly yes"} {
		if !strings.Contains(string(config), want) {
			t.Errorf("ssh config lacks %q", want)
		}
	}
	wrapper, _ := os.ReadFile(filepath.Join(m.sshDir(), "bin", "ssh"))
	if !strings.Contains(string(wrapper), "-F '"+filepath.Join(m.sshDir(), "config")+"'") {
		t.Fatalf("wrapper: %s", wrapper)
	}
	if info, _ := os.Stat(filepath.Join(m.sshDir(), "bin", "ssh")); info.Mode().Perm() != 0700 {
		t.Fatalf("wrapper mode %v", info.Mode())
	}
	if err := m.dockerFor("s00000bad").run(t.Context(), nil, nil, nil, "ps"); err == nil {
		t.Fatal("an unknown server is refused")
	}
}

func TestPortsAreUniquePerServer(t *testing.T) {
	m := testManager(t, newFleet().run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "")
	local := record{instanceSpec: instanceSpec{Name: "a", Port: 15432}}
	remote := instanceSpec{Name: "b", Port: 15432, Server: "s0000000a"}
	if err := checkPorts([]record{local}, remote); err != nil {
		t.Fatalf("another server has its own ports: %v", err)
	}
	if err := checkPorts([]record{local}, instanceSpec{Name: "c", Port: 15432}); status(err) != 409 {
		t.Fatal("the same server refuses a taken port")
	}
	if err := checkPorts(nil, instanceSpec{Name: "d", Port: consolePort(), Server: "s0000000a"}); err != nil {
		t.Fatalf("the console's port is only taken on its own server: %v", err)
	}
}

func TestAnUnreachableServerDoesNotHoldUpTheOthers(t *testing.T) {
	f := newFleet()
	m := testManager(t, f.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "")
	for _, r := range []record{
		{instanceSpec: instanceSpec{Name: "here", Engine: "postgres", Port: 15432}, Project: "p-here"},
		{instanceSpec: instanceSpec{Name: "there", Engine: "postgres", Port: 15432, Server: "s0000000a"}, Project: "p-there"},
	} {
		writeJSON(m.recordPath(r.Name), r)
	}
	f.ps["local"] = "p-here|running|Up 1 minute (healthy)\n"
	f.ps["ssh://openrds@10.0.0.9:22"] = "p-there|running|Up 1 minute (healthy)\n"
	states, err := m.states(t.Context())
	if err != nil || states["p-here"] != "running" || states["p-there"] != "running" {
		t.Fatalf("both servers answer: %v %v", states, err)
	}
	f.down["ssh://openrds@10.0.0.9:22"] = true
	states, err = m.states(t.Context())
	if err != nil || states["p-here"] != "running" || states["p-there"] != "unreachable" {
		t.Fatalf("a server that is down: %v %v", states, err)
	}
	before := f.count("ssh://openrds@10.0.0.9:22", "ps")
	m.states(t.Context())
	if f.count("ssh://openrds@10.0.0.9:22", "ps") != before {
		t.Fatal("a server that just failed is not tried again on every page")
	}
	f.down["local"] = true
	if _, err := m.states(t.Context()); err == nil {
		t.Fatal("the console's own server must answer")
	}
}

func TestLosingAServerIsAnAlert(t *testing.T) {
	f := newFleet()
	m := testManager(t, f.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "")
	m.watchServers(t.Context())
	f.down["ssh://openrds@10.0.0.9:22"] = true
	m.watchServers(t.Context())
	m.watchServers(t.Context())
	f.down["ssh://openrds@10.0.0.9:22"] = false
	m.watchServers(t.Context())
	items, _ := m.readActivity()
	var actions []string
	for _, e := range items {
		if e.Kind == "server" {
			actions = append(actions, e.Action)
		}
	}
	if strings.Join(actions, ", ") != "Server reachable again, Server unreachable" {
		t.Fatalf("one alert when it goes, one when it is back: %v", actions)
	}
	if !strings.Contains(items[1].Detail, "Connection refused") {
		t.Fatalf("the alert says why: %q", items[1].Detail)
	}
}

func TestPrivateAccessListensOnTheServersPrivateAddress(t *testing.T) {
	f := newFleet()
	m := testManager(t, f.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "10.0.0.9")
	addTestServer(t, m, "s0000000b", "openrds@10.0.0.10", "")
	s := instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, Server: "s0000000b", PrivateAccess: true}
	if err := m.preparePrivate(&s); status(err) != 409 {
		t.Fatal("a server without a private address has no private access")
	}
	s.Server = "s0000000a"
	if err := m.preparePrivate(&s); err != nil || allowList(s.PrivateFirewall) != strings.Join(privateRanges, ",") {
		t.Fatalf("private networks are allowed by default: %v %v", s.PrivateFirewall, err)
	}
	var env []string
	m.docker = runFunc(func(ctx context.Context, e []string, in io.Reader, out io.Writer, args ...string) error {
		env = e
		return f.run(ctx, e, in, out, args...)
	})
	m.servers.engines = nil
	os.Setenv("OPENRDS_GATEWAY_IMAGE", "openrds-openrds:latest")
	defer os.Unsetenv("OPENRDS_GATEWAY_IMAGE")
	r := record{instanceSpec: s, Project: "openrds-db-orders"}
	if err := m.compose(t.Context(), r, nil, nil, "up", "private-gateway"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "OPENRDS_PRIVATE_BIND=10.0.0.9") || !slices.Contains(env, "OPENRDS_GATEWAY_TARGET=db:5432") {
		t.Fatalf("env: %v", env)
	}
	cert, _ := m.serverTLS(record{instanceSpec: s, Project: "openrds-db-orders", serverHosts: []string{"10.0.0.9"}})
	if !strings.Contains(strings.Join(tlsHosts(record{instanceSpec: s, serverHosts: []string{"10.0.0.9", "db.example.com"}}), " "), "10.0.0.9 db.example.com") || len(cert.cert) == 0 {
		t.Fatal("certificates name the server's addresses")
	}
}

func TestServersWithDatabasesStay(t *testing.T) {
	m := testManager(t, newFleet().run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "10.0.0.9")
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, Server: "s0000000a", PrivateAccess: true}, Project: "p"}
	writeJSON(m.recordPath(r.Name), r)
	if err := m.removeServer(t.Context(), "s0000000a"); status(err) != 409 || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("a server with databases is not removed: %v", err)
	}
	if _, err := m.updateServer(t.Context(), "s0000000a", serverRequest{Name: "db-a"}); status(err) != 409 {
		t.Fatal("its private address stays while a database uses it")
	}
	if err := m.removeServer(t.Context(), localServer); status(err) != 409 {
		t.Fatal("the console's own server stays")
	}
	os.Remove(m.recordPath(r.Name))
	if err := m.removeServer(t.Context(), "s0000000a"); err != nil {
		t.Fatal(err)
	}
	if len(m.serverList()) != 1 {
		t.Fatal("removed")
	}
}

func TestAddingAServerChecksItsDocker(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("needs ssh-keygen")
	}
	f := newFleet()
	m := testManager(t, f.run)
	key := "openrds@10.0.0.9 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	req := serverRequest{Name: "db-2", Address: "openrds@10.0.0.9", PrivateAddress: "10.0.0.9"}
	if _, err := m.addServer(t.Context(), req); status(err) != 400 {
		t.Fatal("host keys are confirmed first")
	}
	req.HostKeys = []string{strings.TrimPrefix(key, "openrds@")}
	f.down["ssh://openrds@10.0.0.9:22"] = true
	if _, err := m.addServer(t.Context(), req); status(err) != 502 || !strings.Contains(err.Error(), "authorized_keys") {
		t.Fatalf("an unreachable server is not added: %v", err)
	}
	if known, _ := os.ReadFile(filepath.Join(m.sshDir(), "known_hosts")); strings.Contains(string(known), "10.0.0.9") {
		t.Fatal("its host key is not kept")
	}
	f.down["ssh://openrds@10.0.0.9:22"] = false
	s, err := m.addServer(t.Context(), req)
	if err != nil || s.ID == "" || len(m.serverList()) != 2 {
		t.Fatalf("added: %+v %v", s, err)
	}
	if known, _ := os.ReadFile(filepath.Join(m.sshDir(), "known_hosts")); !strings.Contains(string(known), req.HostKeys[0]) {
		t.Fatal("its host key is pinned")
	}
	if pub, _ := os.ReadFile(filepath.Join(m.sshDir(), "id_ed25519.pub")); !strings.HasPrefix(string(pub), "ssh-ed25519 ") {
		t.Fatal("the console has its own key")
	}
	req.Name = "DB-2"
	if _, err := m.addServer(t.Context(), req); status(err) != 409 {
		t.Fatal("names and addresses are unique")
	}
	if !strings.Contains(setupCommand("ssh-ed25519 AAAA openrds"), "usermod -aG docker openrds") {
		t.Fatal("the setup puts the user in the docker group")
	}
}

func TestANewDatabaseGoesToItsServer(t *testing.T) {
	f := newFleet()
	m := testManager(t, f.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "")
	if err := m.provision(t.Context(), instanceSpec{Name: "nowhere", Engine: "postgres", Port: 15432, Server: "s00000bad"}, nil); status(err) != 400 {
		t.Fatal("an unknown server is refused")
	}
	m.provision(t.Context(), instanceSpec{Name: "there", Engine: "postgres", Port: 15432, Server: "s0000000a"}, nil)
	r, err := m.record("there")
	if err != nil || r.Server != "s0000000a" {
		t.Fatalf("recorded on its server: %+v %v", r, err)
	}
	if f.count("ssh://openrds@10.0.0.9:22", "compose") == 0 || f.count("local", "compose") != 0 {
		t.Fatalf("created there: %v", f.calls)
	}
	views, _ := m.serverViews(t.Context())
	if len(views) != 2 || !slices.Equal(views[1].Databases, []string{"there"}) || views[1].Info == nil || !views[1].Reachable {
		t.Fatalf("views: %+v", views)
	}
}

func TestAServersAddressesAreFound(t *testing.T) {
	m := testManager(t, func(_ context.Context, env []string, _ io.Reader, out io.Writer, args ...string) error {
		if slices.Contains(args, "--network") && slices.Contains(args, "ip") {
			fmt.Fprint(out, `1: lo    inet 127.0.0.1/8 scope host lo
2: eth0    inet 203.0.113.10/24 brd 203.0.113.255 scope global eth0
3: eth1    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth1
4: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0
5: br-1a2b    inet 172.18.0.1/16 scope global br-1a2b
6: tailscale0    inet 100.101.102.103/32 scope global tailscale0
`)
		}
		return nil
	})
	addTestServer(t, m, "s0000000a", "openrds@203.0.113.10", "")
	if got := m.detectPrivate(t.Context(), "s0000000a"); !slices.Equal(got, []string{"10.0.0.5", "100.101.102.103"}) {
		t.Fatalf("private addresses, without loopback, Docker bridges or public ones: %v", got)
	}
	for _, c := range []struct {
		address, private, public string
	}{
		{"openrds@203.0.113.10", "10.0.0.5", "203.0.113.10"}, // a public SSH host is the public address
		{"openrds@10.0.0.7", "10.0.0.7", ""},                 // a private SSH host is the private address
		{"openrds@db2.example.com:2222", "10.0.0.5", "db2.example.com"},
		{"openrds@127.0.0.1:22022", "10.0.0.5", ""}, // a tunnel is neither
	} {
		s := m.fillAddresses(t.Context(), server{ID: "s0000000a", Address: c.address})
		if s.PrivateAddress != c.private || s.PublicAddress != c.public {
			t.Errorf("%s: %q %q", c.address, s.PrivateAddress, s.PublicAddress)
		}
	}
	kept := m.fillAddresses(t.Context(), server{ID: "s0000000a", Address: "openrds@203.0.113.10", PrivateAddress: "192.168.1.4", PublicAddress: "x.example.com"})
	if kept.PrivateAddress != "192.168.1.4" || kept.PublicAddress != "x.example.com" {
		t.Fatal("addresses people gave are kept")
	}
}

func TestALeavingPageSaysNothingAboutServers(t *testing.T) {
	f := newFleet()
	m := testManager(t, f.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.9", "")
	f.down["ssh://openrds@10.0.0.9:22"] = true // what a cancelled call looks like
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m.serverViews(ctx)
	m.statesFor(ctx, "io.openrds.database=true")
	if m.recentlyDown("s0000000a") || m.unreachable()["s0000000a"] {
		t.Fatal("a request the browser cancelled does not mark a server unreachable")
	}
}

// The default data directory when running from source is relative. Docker
// over SSH must still find the console's ssh wrapper and key by absolute
// paths: the docker CLI refuses a program found through a relative PATH entry.
func TestARelativeDataDirectoryStillReachesServers(t *testing.T) {
	t.Chdir(t.TempDir())
	m, err := openManager(".openrds", "engines", runFunc(newFleet().run))
	if err != nil {
		t.Fatal(err)
	}
	env, err := m.serverEnv(server{Address: "openrds@10.0.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for _, v := range env {
		if p, ok := strings.CutPrefix(v, "PATH="); ok {
			path = p
		}
	}
	if first, _, _ := strings.Cut(path, ":"); !filepath.IsAbs(first) {
		t.Fatalf("PATH starts with %q", first)
	}
	config, err := os.ReadFile(filepath.Join(m.sshDir(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(config), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), " "); ok && (key == "IdentityFile" || key == "UserKnownHostsFile") && !filepath.IsAbs(strings.Trim(value, `"`)) {
			t.Fatalf("%s is relative: %s", key, value)
		}
	}
}

// Docker Desktop publishes ports on the computer, not in its virtual
// machine, so the machine's address would be a private address nothing can
// bind. It is left empty there, and found on a Linux server.
func TestDockerDesktopHasNoPrivateAddressToOffer(t *testing.T) {
	for _, c := range []struct {
		os   string
		want []string
	}{{"Docker Desktop", nil}, {"Ubuntu 24.04.3 LTS", []string{"192.168.65.3"}}} {
		ran := false
		m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
			switch {
			case args[0] == "info":
				fmt.Fprintf(out, `{"hostname":"h","cpus":8,"memoryBytes":8589934592,"dockerVersion":"29.1.0","architecture":"aarch64","os":%q}`, c.os)
			case slices.Contains(args, "ip"):
				ran = true
				fmt.Fprint(out, "2: eth0    inet 192.168.65.3/24 brd 192.168.65.255 scope global eth0\n")
			}
			return nil
		})
		if got := m.detectPrivate(t.Context(), localServer); !slices.Equal(got, c.want) || ran != (c.want != nil) {
			t.Errorf("%s: %v (looked: %v)", c.os, got, ran)
		}
	}
}
