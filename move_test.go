package main

import (
	"strings"
	"testing"
	"time"
)

func movingSource(t *testing.T) (*manager, *replicaDocker, record) {
	t.Helper()
	d := newReplicaDocker()
	m := testManager(t, d.run)
	addTestServer(t, m, "s0000000a", "openrds@10.0.0.1", "10.0.0.1")
	addTestServer(t, m, "s0000000b", "openrds@10.0.0.2", "10.0.0.2")
	t.Setenv("OPENRDS_GATEWAY_IMAGE", "openrds-openrds:latest")
	src := runningSource(t, m, d, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, Server: "s0000000a", BackupRetentionDays: 7,
		Tags: map[string]string{"env": "prod"}, DeletionProtection: true})
	src.Layout = currentLayout
	src.Created = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	writeJSON(m.recordPath("orders"), src)
	cert, _ := m.serverTLS(record{instanceSpec: src.instanceSpec, Project: src.Project, serverHosts: []string{"10.0.0.1"}})
	d.served = string(cert.cert)
	d.answers = map[string]string{"pg_current_wal_lsn(), '0/0'": "1000\n", "pg_last_wal_replay_lsn(), '0/0'": "1000\n"}
	return m, d, src
}

func TestMovingADatabaseToAnotherServer(t *testing.T) {
	m, d, src := movingSource(t)
	ctx := t.Context()
	for _, c := range []struct {
		req  moveRequest
		code int
	}{
		{moveRequest{Server: "s0000000a"}, 400},
		{moveRequest{Server: "s00000bad"}, 400},
	} {
		if err := m.move(ctx, "orders", c.req); status(err) != c.code {
			t.Errorf("%+v: %v", c.req, err)
		}
	}
	if err := m.move(ctx, "orders", moveRequest{Server: "s0000000b"}); err != nil {
		t.Fatal(err)
	}
	moved, _ := m.record("orders")
	if moved.Server != "s0000000b" || moved.Port != 15432 || moved.Project == src.Project || moved.ReplicaOf != "" || moved.SourceRoute != "" {
		t.Fatalf("the database runs on the other server, from the copy: %+v", moved)
	}
	if !moved.Created.Equal(src.Created) || moved.Tags["env"] != "prod" || !moved.DeletionProtection || moved.BackupRetentionDays != 7 || moved.Password != src.Password {
		t.Fatalf("it keeps its settings, history and owner: %+v", moved)
	}
	all, _ := m.records()
	if len(all) != 1 {
		t.Fatalf("the temporary replica is gone: %d records", len(all))
	}
	log := d.everything()
	for _, want := range []string{"compose --project-name " + src.Project, "down --volumes"} {
		if !strings.Contains(log, want) {
			t.Errorf("the old copy is removed: lacks %q", want)
		}
	}
	if prep := d.input("psql"); !strings.Contains(prep, "default_transaction_read_only = on") || !strings.Contains(prep, "pg_promote(true, 300)") {
		t.Fatalf("writes pause before the copy takes over:\n%s", prep)
	}
	items, _ := m.readActivity()
	if items[0].Action != "Move database" || items[0].Result != "completed" || items[0].Detail != "from db-s0000000a to db-s0000000b" {
		t.Fatalf("events: %+v", items[0])
	}
}

func TestAMoveThatCannotFinishPutsThingsBack(t *testing.T) {
	m, d, src := movingSource(t)
	moveFreeze = time.Second
	defer func() { moveFreeze = time.Minute }()
	d.answers["pg_last_wal_replay_lsn(), '0/0'"] = "900\n" // the copy never gets the last changes
	err := m.move(t.Context(), "orders", moveRequest{Server: "s0000000b"})
	if status(err) != 502 || !strings.Contains(err.Error(), "takes writes again") {
		t.Fatalf("the move stops: %v", err)
	}
	r, _ := m.record("orders")
	if r.Server != "s0000000a" || r.Project != src.Project {
		t.Fatalf("the database stays where it was: %+v", r)
	}
	if all, _ := m.records(); len(all) != 1 {
		t.Fatalf("the copy is removed: %d records", len(all))
	}
	if !strings.Contains(d.input("psql"), "ALTER SYSTEM RESET default_transaction_read_only") {
		t.Fatal("writes come back")
	}
}

func TestWhatCanMove(t *testing.T) {
	m, d, _ := movingSource(t)
	ctx := t.Context()
	if err := m.createReplica(ctx, "orders", replicaRequest{Name: "orders-replica", Port: 15433}); err != nil {
		t.Fatal(err)
	}
	if err := m.move(ctx, "orders", moveRequest{Server: "s0000000b"}); status(err) != 409 || !strings.Contains(err.Error(), "read replicas") {
		t.Fatalf("a database with replicas: %v", err)
	}
	if err := m.move(ctx, "orders-replica", moveRequest{Server: "s0000000b"}); status(err) != 409 || !strings.Contains(err.Error(), "is a read replica") {
		t.Fatalf("a replica: %v", err)
	}
	d.fakeDocker.mu.Lock()
	for p := range d.fakeDocker.running {
		d.fakeDocker.running[p] = false
	}
	d.fakeDocker.mu.Unlock()
	if err := m.remove(ctx, "orders-replica", false); err != nil {
		t.Fatal(err)
	}
	if err := m.move(ctx, "orders", moveRequest{Server: "s0000000b"}); status(err) != 409 || !strings.Contains(err.Error(), "Start orders") {
		t.Fatalf("a stopped database: %v", err)
	}
	if !strings.HasPrefix(moveName("a-very-long-database-name-that-is-40-ch"), "a-very-long-database-name-that-mv-") || len(moveName(strings.Repeat("a", 40))) > 40 {
		t.Fatal(moveName("a-very-long-database-name-that-is-40-ch"))
	}
}
