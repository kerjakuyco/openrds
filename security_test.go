package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The RFC 7677 example exchange must succeed against our verifier: the
// client proof recovers a key that hashes to StoredKey, and the server
// signature matches the one in the RFC.
func TestSCRAMVerifierMatchesRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	v := scramVerifier("pencil", salt, 4096)
	parts := strings.FieldsFunc(strings.TrimPrefix(v, "SCRAM-SHA-256$"), func(r rune) bool { return r == '$' || r == ':' })
	if len(parts) != 4 || parts[0] != "4096" || parts[1] != "W22ZaJ0SNY7soEsUEjb6gQ==" {
		t.Fatalf("verifier format: %s", v)
	}
	stored, _ := base64.StdEncoding.DecodeString(parts[2])
	serverKey, _ := base64.StdEncoding.DecodeString(parts[3])
	nonce := "rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	auth := "n=user,r=rOprNGfwEbeRWgbNEkqO,r=" + nonce + ",s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096,c=biws,r=" + nonce
	mac := func(key []byte) []byte { h := hmac.New(sha256.New, key); h.Write([]byte(auth)); return h.Sum(nil) }
	proof, _ := base64.StdEncoding.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")
	signature := mac(stored)
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ signature[i]
	}
	if sum := sha256.Sum256(clientKey); !bytes.Equal(sum[:], stored) {
		t.Fatal("the RFC client proof does not match StoredKey")
	}
	if got := base64.StdEncoding.EncodeToString(mac(serverKey)); got != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Fatalf("server signature %s", got)
	}
	if pgPassword("pencil") == pgPassword("pencil") {
		t.Fatal("each verifier needs a fresh salt")
	}
}

func TestPasswordsUsersAndRulesAreValidated(t *testing.T) {
	if p, err := choosePassword(""); err != nil || len(p) != 48 {
		t.Fatalf("generated password: %q %v", p, err)
	}
	for _, bad := range []string{"short", "has space in it here", "quote'quote'quote'", strings.Repeat("a", 129), "dollar$dollar$dollar"} {
		if _, err := choosePassword(bad); status(err) != 400 {
			t.Errorf("password %q accepted", bad)
		}
	}
	if p, err := choosePassword("Correct-horse_battery.staple~1"); err != nil || p != "Correct-horse_battery.staple~1" {
		t.Fatalf("a valid custom password was refused: %v", err)
	}
	r := record{instanceSpec: instanceSpec{Name: "orders"}, Users: []dbUser{{Name: "reporting", Access: "read-only"}}}
	for name, want := range map[string]int{"app": 0, "Reporting": 400, "reporting": 409, "postgres": 400, "pg_monitor": 400, "1st": 400, "mysql_x": 400} {
		if got := status(checkDBUser(r, name, "read-write")); got != want {
			t.Errorf("user %q: status %d, want %d", name, got, want)
		}
	}
	if status(checkDBUser(r, "app", "owner")) != 400 {
		t.Error("unknown access level accepted")
	}

	rules, err := normalizeFirewall([]firewallRule{{CIDR: " 203.0.113.7 ", Description: "Office"}, {CIDR: "198.51.100.9/24"}, {CIDR: "203.0.113.7/32"}, {CIDR: "::ffff:192.0.2.1"}, {CIDR: "2001:db8::/32"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.7/32", "198.51.100.0/24", "192.0.2.1/32", "2001:db8::/32"}
	if len(rules) != len(want) {
		t.Fatalf("rules: %+v", rules)
	}
	for i := range want {
		if rules[i].CIDR != want[i] {
			t.Errorf("rule %d: %s, want %s", i, rules[i].CIDR, want[i])
		}
	}
	for _, bad := range []firewallRule{{CIDR: "example.com"}, {CIDR: "10.0.0.0/33"}, {CIDR: "10.0.0.1", Description: "<script>"}} {
		if _, err := normalizeFirewall([]firewallRule{bad}); status(err) != 400 {
			t.Errorf("rule %+v accepted", bad)
		}
	}
	base := instanceSpec{Name: "a", Engine: "postgres", Port: 15432}
	for _, c := range []struct {
		spec instanceSpec
		ok   bool
	}{
		{instanceSpec{PublicAccess: true, PublicPort: 5432}, true},
		{instanceSpec{PublicAccess: true}, false},
		{instanceSpec{PublicAccess: true, PublicPort: 15432}, false},
		{instanceSpec{PublicHost: "db.example.com"}, true},
		{instanceSpec{PublicHost: "203.0.113.7"}, true},
		{instanceSpec{PublicHost: "-bad-.example"}, false},
		{instanceSpec{Firewall: []firewallRule{{CIDR: "10.0.0.0/8"}, {CIDR: "10.0.0.0/8"}}}, false},
	} {
		s := base
		s.PublicAccess, s.PublicPort, s.PublicHost, s.Firewall = c.spec.PublicAccess, c.spec.PublicPort, c.spec.PublicHost, c.spec.Firewall
		if err := checkNetwork(s); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.spec, err)
		}
	}
}

func TestPublicPortsAreReserved(t *testing.T) {
	t.Setenv("OPENRDS_PUBLIC_PORT", "8080")
	all := []record{
		{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, PublicAccess: true, PublicPort: 5432}},
		{instanceSpec: instanceSpec{Name: "cache", Engine: "valkey", Port: 16379, PublicPort: 6379}}, // public access off: not held
	}
	try := func(s instanceSpec) int { return status(checkPorts(all, s)) }
	if try(instanceSpec{Name: "new", Port: 5432}) != 409 || try(instanceSpec{Name: "new", Port: 15433, PublicAccess: true, PublicPort: 15432}) != 409 {
		t.Error("ports held by another database were accepted")
	}
	if try(instanceSpec{Name: "new", Port: 8080}) != 409 {
		t.Error("the console port was accepted")
	}
	if try(instanceSpec{Name: "new", Port: 15433, PublicAccess: true, PublicPort: 6379}) != 0 || try(instanceSpec{Name: "orders", Port: 15432, PublicAccess: true, PublicPort: 5432}) != 0 {
		t.Error("free ports were refused")
	}
	if got := suggestPublicPort(all, instanceSpec{Name: "new", Engine: "postgres", Port: 15433}); got != 5433 {
		t.Errorf("suggested %d, want 5433", got)
	}
}

func TestCertificatesAreReusedAndReissued(t *testing.T) {
	m := testManager(t, nil)
	r := record{instanceSpec: instanceSpec{Name: "orders"}, Project: "openrds-db-test"}
	first, err := m.serverTLS(r)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := m.serverTLS(r)
	if !bytes.Equal(first.cert, again.cert) {
		t.Fatal("an unchanged certificate was reissued")
	}
	r.PublicHost = "db.example.com"
	moved, _ := m.serverTLS(r)
	if bytes.Equal(first.cert, moved.cert) {
		t.Fatal("a new public address did not reissue the certificate")
	}
	reopened, _ := openManager(m.dir, m.templates, nil)
	ca, err := reopened.authority()
	if err != nil || !bytes.Equal(ca.pem, moved.ca) {
		t.Fatal("the certificate authority did not survive a restart")
	}
	block, _ := pem.Decode(moved.cert)
	cert, _ := x509.ParseCertificate(block.Bytes)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(moved.ca)
	for _, host := range []string{"db.example.com", "127.0.0.1", "localhost", "openrds-orders"} {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: host, Roots: pool}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	if users := valkeyUsers(record{Password: "owner-secret", Users: []dbUser{{Name: "reader", Access: "read-only", Password: "reader-secret"}}}); strings.Contains(users, "secret") ||
		!strings.Contains(users, "user default on #"+valkeyHash("owner-secret")+" ~* &* +@all") || !strings.Contains(users, "user reader on #"+valkeyHash("reader-secret")) {
		t.Fatalf("valkey users file:\n%s", users)
	}
}

func TestGatewayForwardsOnlyAllowedSources(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	try := func(allow string) (string, string) {
		var log bytes.Buffer
		g, err := newGateway(echo.Addr().String(), allow, &log)
		if err != nil {
			t.Fatal(err)
		}
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer ln.Close()
		go g.serve(ln)
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("ping"))
		buf := make([]byte, 4)
		n, _ := io.ReadFull(c, buf)
		time.Sleep(20 * time.Millisecond)
		g.logMu.Lock()
		defer g.logMu.Unlock()
		return string(buf[:n]), log.String()
	}
	if got, log := try("10.0.0.0/8,127.0.0.1"); got != "ping" || !strings.Contains(log, `"allowed":true`) || !strings.Contains(log, `"ip":"127.0.0.1"`) {
		t.Fatalf("allowed source: %q, log %q", got, log)
	}
	if got, log := try("10.0.0.0/8"); got != "" || !strings.Contains(log, `"allowed":false`) {
		t.Fatalf("denied source got %q, log %q", got, log)
	}
	if got, _ := try(""); got != "" {
		t.Fatal("an empty allowlist must deny everyone")
	}
	attempts := parseAttempts([]byte(`{"at":"2026-11-22T01:00:00Z","ip":"198.51.100.4","allowed":false,"count":3}
not json
{"at":"2026-11-22T01:05:00Z","ip":"198.51.100.4","allowed":true,"count":1}
{"at":"2026-11-22T01:02:00Z","ip":"203.0.113.9","allowed":false,"count":1}
`), 10)
	if len(attempts) != 2 || attempts[0].IP != "198.51.100.4" || !attempts[0].Allowed || attempts[0].Attempts != 4 || attempts[1].Allowed {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestCredentialChangesStayOffCommandLines(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "valkey"} {
		t.Run(engine, func(t *testing.T) {
			f := newFakeDocker()
			m := testManager(t, f.run)
			ctx := t.Context()
			if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: engine, Port: 15432}, nil); err != nil {
				t.Fatal(err)
			}
			before, _ := m.record("orders")
			password, err := m.changePassword(ctx, "orders", "", "")
			if err != nil {
				t.Fatal(err)
			}
			after, _ := m.record("orders")
			if after.Password != password || password == before.Password {
				t.Fatal("the new owner password was not kept")
			}
			app, err := m.createUser(ctx, "orders", "app", "read-write", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.createUser(ctx, "orders", "app", "read-only", ""); status(err) != 409 {
				t.Fatal("duplicate user accepted")
			}
			if err := m.deleteUser(ctx, "orders", "app"); err != nil {
				t.Fatal(err)
			}
			final, _ := m.record("orders")
			if len(final.Users) != 0 {
				t.Fatal("deleted user still stored")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, args := range f.args {
				line := strings.Join(args, " ")
				if strings.Contains(line, password) || strings.Contains(line, app) {
					t.Fatalf("a password reached a command line: %s", line)
				}
			}
			inputs := strings.Join(f.inputs, "\n")
			switch engine {
			case "postgres":
				if strings.Contains(inputs, password) || !strings.Contains(inputs, "SCRAM-SHA-256$4096:") || !strings.Contains(inputs, "GRANT pg_write_all_data") {
					t.Fatalf("postgres statements: %s", inputs)
				}
			case "mysql":
				if !strings.Contains(inputs, "IDENTIFIED BY '"+password+"'") || !strings.Contains(inputs, "INSERT, UPDATE, DELETE ON app.*") {
					t.Fatalf("mysql statements: %s", inputs)
				}
			case "valkey":
				if !strings.HasPrefix(f.inputs[0], before.Password+"\n") || !strings.Contains(inputs, "resetpass #"+valkeyHash(password)) || !strings.Contains(inputs, "user default on #"+valkeyHash(password)) {
					t.Fatalf("valkey commands must authenticate with the old password and store hashes: %s", inputs)
				}
			}
		})
	}
}

func TestCredentialRevealsAreAuditedAndNeedARecentContainer(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	token := randomHex(32)
	h := handler(m, testAuth(t, m, token))
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("/api/instances/orders/connection"); w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"openrds"`) {
		t.Fatalf("connection: %d %s", w.Code, w.Body)
	}
	events, _ := m.readActivity()
	if len(events) == 0 || events[0].Action != "Reveal credentials" || events[0].Actor != "API key" {
		t.Fatalf("reveal not recorded: %+v", events[:1])
	}
	if w := get("/api/tls/ca.pem"); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("CA download: %d", w.Code)
	}
	r, _ := m.record("orders")
	r.Layout = 0 // started by an earlier version
	m.save(r)
	if _, err := m.changePassword(ctx, "orders", "", ""); status(err) != 409 || !strings.Contains(err.Error(), "Reboot orders once") {
		t.Fatalf("an old container must be rebooted first: %v", err)
	}
}

func TestGatewayFollowsPublicAccess(t *testing.T) {
	t.Setenv("OPENRDS_GATEWAY_IMAGE", "openrds:test")
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	spec := instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, PublicAccess: true, PublicPort: 25432, Firewall: []firewallRule{{CIDR: "203.0.113.0/24"}}}
	if err := m.provision(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	calls := func(words ...string) int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, args := range f.args {
			if strings.HasSuffix(strings.Join(args, " "), strings.Join(words, " ")) {
				n++
			}
		}
		return n
	}
	if calls("up", "--detach", "--wait", "--wait-timeout", "60", "gateway") != 1 || f.lastUp("OPENRDS_GATEWAY_ALLOW") != "203.0.113.0/24" || f.lastUp("OPENRDS_GATEWAY_PUBLISH") != "25432" {
		t.Fatal("a public database must start its gateway with the allowlist")
	}
	if _, err := m.modify(ctx, "orders", modification{Firewall: &[]firewallRule{{CIDR: "198.51.100.7"}}}); err != nil {
		t.Fatal(err)
	}
	if f.lastUp("OPENRDS_GATEWAY_ALLOW") != "198.51.100.7/32" {
		t.Fatal("a rule change must reach the gateway at once")
	}
	if err := m.change(ctx, "orders", "stop"); err != nil || calls("stop", "--timeout", "45", "db", "gateway") != 1 {
		t.Fatalf("stopping must stop the gateway too: %v", err)
	}
	if err := m.change(ctx, "orders", "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.modify(ctx, "orders", modification{PublicAccess: ptr(false), MemoryMiB: ptr(512), ApplyImmediately: true}); err != nil {
		t.Fatal(err)
	}
	if calls("rm", "--stop", "--force", "gateway") != 1 {
		t.Fatal("turning public access off during a restart must remove the gateway")
	}
	events, _ := m.readActivity()
	if !slices.ContainsFunc(events, func(e event) bool { return e.Action == "Change network access" }) {
		t.Fatal("a rules-only change should read as a network change in Events")
	}
}
