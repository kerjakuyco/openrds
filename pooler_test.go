package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPoolerSettingsAreChecked(t *testing.T) {
	pg := instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}
	refused := []instanceSpec{
		{Name: "cache", Engine: "valkey", Port: 16379, Pooler: &poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 20, MaxClients: 100, Port: 6432}},
		withPooler(pg, poolerSpec{Enabled: true, Mode: "statement", PoolSize: 20, MaxClients: 100, Port: 6432}),
		withPooler(pg, poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 0, MaxClients: 100, Port: 6432}),
		withPooler(pg, poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 20, MaxClients: maxPoolerClients + 1, Port: 6432}),
		withPooler(pg, poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 20, MaxClients: 100, Port: 15432}), // the database's own port
		withPooler(pg, poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 20, MaxClients: 100, Port: 80}),
	}
	for _, s := range refused {
		if err := checkSpec(s); status(err) != 400 {
			t.Errorf("accepted %+v: %v", *s.Pooler, err)
		}
	}
	others := []record{{instanceSpec: instanceSpec{Name: "billing", Engine: "postgres", Port: 15433, Pooler: &poolerSpec{Enabled: true, Port: 6432}}}}
	s := withPooler(pg, poolerSpec{Enabled: true})
	normalizePooler(&s, others)
	if p := s.Pooler; p.Mode != "transaction" || p.PoolSize != 20 || p.MaxClients != 1000 || p.Port != 6433 {
		t.Fatalf("defaults, and the next free port: %+v", *p)
	}
	if err := checkSpec(s); err != nil {
		t.Fatal(err)
	}
	taken := s
	taken.Pooler = &poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 20, MaxClients: 100, Port: 6432}
	if err := checkPorts(others, taken); status(err) != 409 {
		t.Fatal("a pooler port another database uses must be refused")
	}
	off := withPooler(pg, poolerSpec{Enabled: false, Port: 7000})
	normalizePooler(&off, nil)
	if off.Pooler != nil || len(off.hostPorts()) != 1 {
		t.Fatal("a pooler turned off holds no port")
	}
}

func withPooler(s instanceSpec, p poolerSpec) instanceSpec {
	s.Pooler = &p
	return s
}

func TestPoolerConfiguration(t *testing.T) {
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Parameters: map[string]string{"max_connections": "200"},
		Pooler: &poolerSpec{Enabled: true, Mode: "transaction", PoolSize: 25, MaxClients: 5000, Port: 6432}},
		Password: "ownerpassword0123456789", Users: []dbUser{{Name: "reporting", Password: "reportpassword0123456"}}}
	ini := poolerIni(r)
	for _, want := range []string{"* = host=db port=5432", "pool_mode = transaction", "default_pool_size = 25", "max_client_conn = 5000",
		"max_db_connections = 190", "max_prepared_statements = 200", "auth_type = scram-sha-256", "client_tls_sslmode = prefer", "server_tls_sslmode = prefer"} {
		if !strings.Contains(ini, want+"\n") {
			t.Errorf("pgbouncer.ini lacks %q:\n%s", want, ini)
		}
	}
	r.RequireTLS, r.Parameters = true, nil
	ini = poolerIni(r)
	if !strings.Contains(ini, "client_tls_sslmode = require\n") || !strings.Contains(ini, "server_tls_sslmode = require\n") || !strings.Contains(ini, "max_db_connections = 90\n") {
		t.Fatalf("TLS required, and PostgreSQL's default of 100 connections:\n%s", ini)
	}
	r.Parameters = map[string]string{"max_connections": "12"}
	if !strings.Contains(poolerIni(r), "max_db_connections = 6\n") {
		t.Fatal("a small server keeps half its connections for the pooler")
	}
	if got := poolerUsers(r); got != "\"openrds\" \"ownerpassword0123456789\"\n\"reporting\" \"reportpassword0123456\"\n" {
		t.Fatalf("users file: %q", got)
	}
	if !strings.Contains(strings.Join(tlsHosts(r), " "), "openrds-orders-pooler") {
		t.Fatal("the certificate must name the pooler, so clients can verify it")
	}
}

func overrideLabels(t *testing.T, m *manager, r record) map[string]string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Labels map[string]string `json:"labels"`
		} `json:"services"`
	}
	data, err := os.ReadFile(m.overridePath(r))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for name, svc := range doc.Services {
		labels[name] = svc.Labels["io.openrds.secrets"]
	}
	return labels
}

func TestPoolerFollowsTheDatabase(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	ups := f.upCount()
	res, err := m.modify(ctx, "orders", modification{Pooler: &poolerSpec{Enabled: true}})
	if err != nil || res.Restarted || res.PendingRestart {
		t.Fatalf("turning pooling on: %+v %v", res, err)
	}
	r, _ := m.record("orders")
	if !r.Pooler.on() || r.Pooler.Port != 6432 || r.Pooler.Mode != "transaction" {
		t.Fatalf("record: %+v", r.Pooler)
	}
	last := f.args[len(f.args)-1]
	if composeVerb(last) != "up" || last[len(last)-1] != "pooler" || f.upCount() != ups+1 || f.lastUp("OPENRDS_POOLER_PORT") != "6432" ||
		f.lastUp("OPENRDS_POOLER_ALIAS") != "openrds-orders-pooler" || !strings.Contains(f.lastUp("OPENRDS_POOLER_INI"), "pool_mode = transaction") {
		t.Fatalf("only the pooler starts, with its settings: %v", last)
	}
	before := overrideLabels(t, m, r)

	// A new user reaches the running pooler without recreating anything.
	if _, err := m.createUser(ctx, "orders", "reporting", "read-only", ""); err != nil {
		t.Fatal(err)
	}
	reload := f.args[len(f.args)-1]
	if composeVerb(reload) != "exec" || !slicesContain(reload, "pooler") || !strings.Contains(f.inputs[len(f.inputs)-1], `"reporting"`) {
		t.Fatalf("the users file is rewritten in the pooler: %v %q", reload, f.inputs[len(f.inputs)-1])
	}
	r, _ = m.record("orders")
	m.compose(ctx, r, nil, nil, "ps") // rewrites the override from the record
	after := overrideLabels(t, m, r)
	if after["db"] != before["db"] || after["pooler"] == before["pooler"] {
		t.Fatalf("a user change must recreate only the pooler, never the database: %v → %v", before, after)
	}

	if err := m.change(ctx, "orders", "stop"); err != nil || !slicesContain(f.args[len(f.args)-1], "pooler") {
		t.Fatalf("stopping stops the pooler too: %v", err)
	}
	if err := m.change(ctx, "orders", "start"); err != nil {
		t.Fatal(err)
	}
	if last := f.args[len(f.args)-1]; composeVerb(last) != "up" || last[len(last)-1] != "pooler" {
		t.Fatalf("starting brings the pooler back after the database: %v", last)
	}

	if _, err := m.modify(ctx, "orders", modification{Pooler: &poolerSpec{Enabled: true, PoolSize: 50}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.record("orders"); r.Pooler.Port != 6432 || r.Pooler.PoolSize != 50 {
		t.Fatalf("a settings change keeps the port: %+v", r.Pooler)
	}
	if _, err := m.modify(ctx, "orders", modification{Pooler: &poolerSpec{Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	last = f.args[len(f.args)-1]
	if r, _ := m.record("orders"); r.Pooler != nil || composeVerb(last) != "rm" || !slicesContain(last, "pooler") {
		t.Fatalf("turning pooling off removes the pooler: %+v %v", r.Pooler, last)
	}
	items, _ := m.readActivity()
	if items[0].Action != "Change connection pooling" || items[0].Result != "completed" {
		t.Fatalf("events: %+v", items[0])
	}
}

func slicesContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestAMissingPoolerImageIsExplained(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	m.docker = runFunc(func(c context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
		if args[0] == "image" {
			return errors.New("No such image: openrds-pooler:local")
		}
		return f.run(c, env, in, out, args...)
	})
	_, err := m.modify(ctx, "orders", modification{Pooler: &poolerSpec{Enabled: true}})
	if err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("the missing image must be named with the fix: %v", err)
	}
	if r, _ := m.record("orders"); r.Pooler != nil {
		t.Fatal("a pooler that could not start must not stay in the record")
	}
}
