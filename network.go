package main

// Public access. A database stays on the VPS loopback interface; turning on
// public access adds a gateway container that publishes a second port on
// every interface and forwards only connections from allowed addresses.
// Docker publishes ports ahead of host firewalls such as ufw, so the gateway
// is where the allowlist is enforced.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxFirewallRules = 50
	gatewayPort      = "7000" // inside the gateway container
)

var (
	hostPattern        = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	descriptionPattern = regexp.MustCompile(`^[\p{L}\p{N} _.:/=+\-@'()]{0,100}$`)
)

type firewallRule struct {
	CIDR        string `json:"cidr"`
	Description string `json:"description,omitempty"`
}

// An address or range; a single address becomes a /32 or /128 range.
func parseSource(text string) (netip.Prefix, error) {
	text = strings.TrimSpace(text)
	if p, err := netip.ParsePrefix(text); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(text)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Rules in canonical form, without duplicates.
func normalizeFirewall(rules []firewallRule) ([]firewallRule, error) {
	if len(rules) > maxFirewallRules {
		return nil, fail(400, fmt.Sprintf("Use at most %d firewall rules.", maxFirewallRules))
	}
	var out []firewallRule
	for _, rule := range rules {
		p, err := parseSource(rule.CIDR)
		if err != nil {
			return nil, fail(400, fmt.Sprintf("%q is not an IP address or range. Use forms such as 203.0.113.7 or 203.0.113.0/24.", rule.CIDR))
		}
		desc := strings.TrimSpace(rule.Description)
		if !descriptionPattern.MatchString(desc) {
			return nil, fail(400, "Rule descriptions have up to 100 letters, digits, spaces and _ . : / = + - @ ' ( ).")
		}
		if slices.ContainsFunc(out, func(r firewallRule) bool { return r.CIDR == p.String() }) {
			continue
		}
		out = append(out, firewallRule{p.String(), desc})
	}
	return out, nil
}

func checkNetwork(s instanceSpec) error {
	if s.PublicAccess || s.PublicPort != 0 {
		if s.PublicPort < 1024 || s.PublicPort > 65535 {
			return fail(400, "Choose a public port between 1024 and 65535.")
		}
		if s.PublicPort == s.Port {
			return fail(400, "Use a public port that differs from the loopback port.")
		}
	}
	if s.PublicHost != "" && net.ParseIP(s.PublicHost) == nil && (len(s.PublicHost) > 253 || !hostPattern.MatchString(s.PublicHost)) {
		return fail(400, "Use a hostname such as db.example.com or an IP address for the public address.")
	}
	rules, err := normalizeFirewall(s.Firewall)
	if err != nil {
		return err
	}
	if len(rules) != len(s.Firewall) {
		return fail(400, "Each firewall source can appear only once.")
	}
	return nil
}

// Canonical firewall rules, and a free public port when public access is
// requested without one.
func (m *manager) prepareNetwork(s *instanceSpec) error {
	rules, err := normalizeFirewall(s.Firewall)
	if err != nil {
		return err
	}
	if err := m.preparePrivate(s); err != nil {
		return err
	}
	s.Firewall, s.PublicHost = rules, strings.TrimSpace(s.PublicHost)
	if s.PublicAccess && s.PublicPort == 0 || s.Pooler != nil {
		all, err := m.records()
		if err != nil {
			return err
		}
		if s.PublicAccess && s.PublicPort == 0 {
			s.PublicPort = suggestPublicPort(all, *s)
		}
		normalizePooler(s, all)
	}
	return nil
}

// The console's port on the host, which a public port must not reuse.
func consolePort() int {
	port, err := strconv.Atoi(env("OPENRDS_PUBLIC_PORT", listenPort()))
	if err != nil {
		return 8080
	}
	return port
}

func allowList(rules []firewallRule) string {
	cidrs := make([]string, 0, len(rules))
	for _, r := range rules {
		cidrs = append(cidrs, r.CIDR)
	}
	return strings.Join(cidrs, ",")
}

// The gateway runs the control plane's own image. In a container the image
// is read from Docker; elsewhere, such as in development, set
// OPENRDS_GATEWAY_IMAGE.
func (m *manager) gatewayImage(ctx context.Context) (string, error) {
	if image := os.Getenv("OPENRDS_GATEWAY_IMAGE"); image != "" {
		return image, nil
	}
	m.mu.Lock()
	image := m.gateway
	m.mu.Unlock()
	if image != "" {
		return image, nil
	}
	host, _ := os.Hostname()
	var out bytes.Buffer
	if err := m.docker.run(ctx, nil, nil, &out, "inspect", "--format", "{{.Config.Image}}", host); err != nil {
		return "", fmt.Errorf("finding the OpenRDS image for the gateway (set OPENRDS_GATEWAY_IMAGE): %w", err)
	}
	image = strings.TrimSpace(out.String())
	m.mu.Lock()
	m.gateway = image
	m.mu.Unlock()
	return image, nil
}

// Private access needs an address on the database's server. Turning it on
// without sources allows the usual private ranges.
func (m *manager) preparePrivate(s *instanceSpec) error {
	rules, err := normalizeFirewall(s.PrivateFirewall)
	if err != nil {
		return err
	}
	s.PrivateFirewall = rules
	if !s.PrivateAccess {
		return nil
	}
	srv, ok := m.server(s.Server)
	if !ok || srv.PrivateAddress == "" {
		return fail(409, "Set the private address of "+cmp.Or(srv.Name, "this server")+" on the Servers page first.")
	}
	if len(s.PrivateFirewall) == 0 {
		for _, cidr := range privateRanges {
			s.PrivateFirewall = append(s.PrivateFirewall, firewallRule{CIDR: cidr, Description: "Private networks"})
		}
	}
	return nil
}

// Start, update or remove the private gateway to match the record and its
// replicas on other servers.
func (m *manager) applyPrivateGateway(ctx context.Context, previous, next record, databaseLive bool) error {
	if m.servesPrivately(next) {
		if !databaseLive {
			return nil
		}
		return m.compose(ctx, next, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "60", "private-gateway")
	}
	return m.removeService(ctx, next, "private-gateway")
}

// Remove a service's container directly, for one its Compose files no longer include.
func (m *manager) removeService(ctx context.Context, r record, service string) error {
	var out bytes.Buffer
	if err := m.on(r).run(ctx, nil, nil, &out, "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+r.Project,
		"--filter", "label=com.docker.compose.service="+service); err != nil {
		return err
	}
	ids := strings.Fields(out.String())
	if len(ids) == 0 {
		return nil
	}
	return m.on(r).run(ctx, nil, nil, io.Discard, append([]string{"rm", "--force"}, ids...)...)
}

// Start, update or remove the gateway to match the record. Compose
// recreates it when the allowlist or port changes.
func (m *manager) applyGateway(ctx context.Context, previous, next record, databaseLive bool) error {
	if next.PublicAccess && databaseLive {
		return m.compose(ctx, next, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "60", "gateway")
	}
	if previous.PublicAccess {
		return m.compose(ctx, previous, nil, nil, "rm", "--stop", "--force", "gateway")
	}
	return nil
}

type connectionAttempt struct {
	IP       string    `json:"ip"`
	Allowed  bool      `json:"allowed"`
	Attempts int       `json:"attempts"`
	Last     time.Time `json:"lastAt"`
}

// Recent sources that tried the public port, newest first, from the
// gateway's log. Denied sources can be allowed from the console.
func (m *manager) connectionAttempts(ctx context.Context, name string) ([]connectionAttempt, error) {
	r, err := m.record(name)
	if err != nil {
		return nil, err
	}
	out := []connectionAttempt{}
	if !r.PublicAccess {
		return out, nil
	}
	logs := tailBuffer{limit: 256 << 10}
	if err := m.compose(ctx, r, nil, &logs, "logs", "--no-color", "--no-log-prefix", "--tail", "2000", "gateway"); err != nil {
		return out, nil // no gateway container yet
	}
	return parseAttempts(logs.data, 25), nil
}

func parseAttempts(data []byte, limit int) []connectionAttempt {
	byIP := map[string]*connectionAttempt{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var line struct {
			At      time.Time `json:"at"`
			IP      string    `json:"ip"`
			Allowed bool      `json:"allowed"`
			Count   int       `json:"count"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil || line.IP == "" {
			continue
		}
		a := byIP[line.IP]
		if a == nil {
			a = &connectionAttempt{IP: line.IP}
			byIP[line.IP] = a
		}
		a.Attempts += max(line.Count, 1)
		if !line.At.Before(a.Last) {
			a.Last, a.Allowed = line.At, line.Allowed
		}
	}
	out := make([]connectionAttempt, 0, len(byIP))
	for _, a := range byIP {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---- the gateway process (openrds gateway)

type gateway struct {
	target  string
	allowed []netip.Prefix
	log     io.Writer
	logMu   sync.Mutex
	seen    map[string]*logState
	every   time.Duration
}

type logState struct {
	last  time.Time
	count int
}

func newGateway(target, allow string, log io.Writer) (*gateway, error) {
	g := &gateway{target: target, log: log, seen: map[string]*logState{}, every: 30 * time.Second}
	for _, text := range strings.Split(allow, ",") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		p, err := parseSource(text)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed source %q", text)
		}
		g.allowed = append(g.allowed, p)
	}
	return g, nil
}

func (g *gateway) permits(a netip.Addr) bool {
	a = a.Unmap()
	return slices.ContainsFunc(g.allowed, func(p netip.Prefix) bool { return p.Contains(a) })
}

// One log line per source and outcome every 30 seconds, with a count, so
// a busy client or a port scanner cannot flood the log.
func (g *gateway) record(a netip.Addr, allowed bool, now time.Time) {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	key := a.String() + strconv.FormatBool(allowed)
	s := g.seen[key]
	if s == nil {
		if len(g.seen) > 10000 {
			clear(g.seen)
		}
		s = &logState{}
		g.seen[key] = s
	}
	s.count++
	if now.Sub(s.last) < g.every {
		return
	}
	line, _ := json.Marshal(map[string]any{"at": now.UTC().Format(time.RFC3339), "ip": a.String(), "allowed": allowed, "count": s.count})
	fmt.Fprintf(g.log, "%s\n", line)
	s.last, s.count = now, 0
}

func (g *gateway) serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go g.handle(c)
	}
}

func (g *gateway) handle(c net.Conn) {
	defer c.Close()
	addr, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return
	}
	ok := g.permits(addr.Addr())
	g.record(addr.Addr().Unmap(), ok, time.Now())
	if !ok {
		return
	}
	up, err := net.DialTimeout("tcp", g.target, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
}

func runGateway(out, errOut io.Writer) int {
	g, err := newGateway(os.Getenv("OPENRDS_GATEWAY_TARGET"), os.Getenv("OPENRDS_GATEWAY_ALLOW"), out)
	if err == nil && g.target == "" {
		err = errors.New("OPENRDS_GATEWAY_TARGET is not set")
	}
	var ln net.Listener
	if err == nil {
		ln, err = net.Listen("tcp", ":"+gatewayPort)
	}
	if err != nil {
		fmt.Fprintln(errOut, "openrds gateway:", err)
		return 1
	}
	fmt.Fprintf(errOut, "openrds gateway: forwarding to %s for %d allowed sources\n", g.target, len(g.allowed))
	if err := g.serve(ln); err != nil {
		fmt.Fprintln(errOut, "openrds gateway:", err)
		return 1
	}
	return 0
}
