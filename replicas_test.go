package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake Docker for replicas: containers have IDs, engine clients answer
// with what replicas report, and every call is kept.
type replicaDocker struct {
	*fakeDocker
	mu      sync.Mutex
	calls   [][]string
	inputs  map[string]string // stdin by command line
	answer  string            // what docker exec engine clients print
	answers map[string]string // or this, for input that contains the key
}

func newReplicaDocker() *replicaDocker {
	return &replicaDocker{fakeDocker: newFakeDocker(), inputs: map[string]string{}, answer: "streaming|0\n"}
}

func (d *replicaDocker) run(ctx context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
	d.mu.Lock()
	d.calls = append(d.calls, args)
	d.mu.Unlock()
	keep := func() {
		if in == nil {
			return
		}
		data, _ := io.ReadAll(in)
		d.mu.Lock()
		d.inputs[strings.Join(args, " ")] += string(data)
		d.mu.Unlock()
	}
	switch args[0] {
	case "inspect", "network":
		return nil
	case "exec": // engine clients
		data, _ := io.ReadAll(in)
		d.mu.Lock()
		defer d.mu.Unlock()
		answer := d.answer
		for k, v := range d.answers {
			if strings.Contains(string(data), k) {
				answer = v
			}
		}
		_, err := io.WriteString(out, answer)
		return err
	case "compose":
		switch composeVerb(args) {
		case "ps":
			if slices.Contains(args, "--quiet") {
				fmt.Fprintln(out, strings.TrimPrefix(args[2], "openrds-db-"))
				return nil
			}
		case "logs":
			return nil
		case "run":
			keep()
			return nil
		case "exec":
			d.fakeDocker.mu.Lock()
			running := d.fakeDocker.running[args[2]]
			d.fakeDocker.mu.Unlock()
			if !running {
				return fmt.Errorf("service \"db\" is not running")
			}
			keep()
			if out != nil {
				switch {
				case slices.Contains(args, "pg_basebackup"):
					io.WriteString(out, "base backup")
				case strings.Contains(args[len(args)-1], "SELECT @@log_bin"), strings.HasSuffix(args[len(args)-1], "-N -B"):
					io.WriteString(out, "1\n")
				}
			}
			return nil
		}
	}
	return d.fakeDocker.run(ctx, env, in, out, args...)
}

func (d *replicaDocker) called(prefix ...string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func (d *replicaDocker) input(scriptPart string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var all []string
	for script, in := range d.inputs {
		if strings.Contains(script, scriptPart) {
			all = append(all, in)
		}
	}
	return strings.Join(all, "\n")
}

func (d *replicaDocker) everything() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b strings.Builder
	for _, c := range d.calls {
		b.WriteString(strings.Join(c, " ") + "\n")
	}
	for _, in := range d.inputs {
		b.WriteString(in + "\n")
	}
	return b.String()
}

func runningSource(t *testing.T, m *manager, d *replicaDocker, spec instanceSpec) record {
	t.Helper()
	if err := m.provision(t.Context(), spec, nil); err != nil {
		t.Fatal(err)
	}
	r, err := m.record(spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReplicasHaveTheirSourcesUsers(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	src := record{instanceSpec: instanceSpec{Name: "cache", Engine: "valkey", Port: 16379, RequireTLS: true}, Project: "openrds-db-cache",
		Password: "owner-secret", ReplicationPassword: "replication-secret", Users: []dbUser{{Name: "reader", Access: "read-only", Password: "reader-secret"}}}
	rep := record{instanceSpec: instanceSpec{Name: "cache-replica", Engine: "valkey", Port: 16380}, Project: "openrds-db-rep", Password: "stale", ReplicaOf: "cache"}
	writeJSON(m.recordPath(src.Name), src)
	writeJSON(m.recordPath(rep.Name), rep)
	got, _ := m.record("cache-replica")
	if got.Password != "owner-secret" || len(got.Users) != 1 || got.ReplicationPassword != "replication-secret" || !got.sourceTLS {
		t.Fatalf("a replica reads its source's credentials: %+v", got)
	}
	all, _ := m.records()
	if r := all[slices.IndexFunc(all, func(r record) bool { return r.Name == "cache-replica" })]; r.Password != "owner-secret" {
		t.Fatal("records() gives replicas their source's credentials too")
	}
	users := valkeyUsers(got)
	for _, want := range []string{"replicaof openrds-cache 6379", "masteruser openrds_replication", "masterauth replication-secret", "tls-replication yes", "user reader on #"} {
		if !strings.Contains(users, want) {
			t.Errorf("the replica's Valkey file lacks %q:\n%s", want, users)
		}
	}
	if strings.Contains(users, "\nuser openrds_replication ") {
		t.Error("only sources accept the replication user")
	}
	if users := valkeyUsers(src); !strings.Contains(users, "user openrds_replication on #"+valkeyHash("replication-secret")+" +psync +replconf +ping") || strings.Contains(users, "replicaof") {
		t.Errorf("a source accepts the replication user, and only to replicate:\n%s", users)
	}
	if !strings.Contains(pgHBA(record{}), "hostssl replication openrds_replication all scram-sha-256") {
		t.Fatal("PostgreSQL replicas connect with TLS and a password")
	}
	if err := checkDBUser(record{}, "openrds_replication", "read-only"); status(err) != 400 {
		t.Fatal("the replication user's name is reserved")
	}

	pg := strings.Join(record{instanceSpec: instanceSpec{Engine: "postgres", DiskGiB: 20}, ReplicationPassword: "x"}.command(), " ")
	if !strings.Contains(pg, "max_slot_wal_keep_size=5GB") {
		t.Fatalf("slots keep at most a quarter of the source's disk: %s", pg)
	}
	if strings.Contains(strings.Join(record{instanceSpec: instanceSpec{Engine: "postgres"}}.command(), " "), "max_slot_wal_keep_size") {
		t.Fatal("databases without replicas keep their command line")
	}
	my := strings.Join(record{instanceSpec: instanceSpec{Engine: "mysql"}, ReplicaOf: "orders", ServerID: 4242}.command(), " ")
	for _, want := range []string{"--server-id=4242", "--relay-log-recovery=ON", "--event-scheduler=OFF"} {
		if !strings.Contains(my, want) {
			t.Errorf("a MySQL replica's command lacks %s: %s", want, my)
		}
	}
	if id := serverID(); id < 2 {
		t.Fatalf("server ID %d", id)
	}
}

func TestReplicationRules(t *testing.T) {
	src := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Parameters: map[string]string{"max_connections": "300"}}}
	rep := record{instanceSpec: instanceSpec{Name: "orders-replica", Engine: "postgres", Parameters: map[string]string{"max_connections": "300"}}, ReplicaOf: "orders"}
	all := []record{src, rep}
	if err := checkReplication(all, rep); err != nil {
		t.Fatal(err)
	}
	lower := rep
	lower.Parameters = map[string]string{"max_connections": "200"}
	if err := checkReplication(all, lower); status(err) != 400 || !strings.Contains(err.Error(), "at least 300") {
		t.Fatalf("a standby keeps max_connections at least the primary's: %v", err)
	}
	higher := src
	higher.Parameters = map[string]string{"max_connections": "400"}
	if err := checkReplication(all, higher); status(err) != 400 || !strings.Contains(err.Error(), "orders-replica") {
		t.Fatalf("raise it on the replicas first: %v", err)
	}
	archiving := rep
	archiving.PITR = true
	if err := checkReplication(all, archiving); status(err) != 400 {
		t.Fatal("replicas don't archive WAL")
	}

	d := newReplicaDocker()
	m := testManager(t, d.run)
	ctx := t.Context()
	source := runningSource(t, m, d, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, DiskGiB: 0})
	source.Layout = currentLayout
	writeJSON(m.recordPath("orders"), source)
	for _, c := range []struct {
		req  replicaRequest
		want int
	}{
		{replicaRequest{Name: "Bad Name", Port: 15433}, 400},
		{replicaRequest{Name: "orders-replica", Port: 15432}, 409}, // the source's port
	} {
		if err := m.createReplica(ctx, "orders", c.req); status(err) != c.want {
			t.Errorf("%+v: %v", c.req, err)
		}
	}
	if err := m.createReplica(ctx, "orders", replicaRequest{Name: "orders-replica", Port: 15433}); err != nil {
		t.Fatal(err)
	}
	if err := m.createReplica(ctx, "orders-replica", replicaRequest{Name: "deeper", Port: 15434}); status(err) != 409 {
		t.Fatal("no replicas of replicas")
	}
	if err := m.remove(ctx, "orders", false); status(err) != 409 || !strings.Contains(err.Error(), "orders-replica") {
		t.Fatalf("a source with replicas stays: %v", err)
	}
	if err := m.upgrade(ctx, "orders", "19"); status(err) != 409 {
		t.Fatalf("no upgrade with replicas: %v", err)
	}
	if err := m.upgrade(ctx, "orders-replica", "19"); status(err) != 409 {
		t.Fatalf("replicas run their source's version: %v", err)
	}
	if _, err := m.createUser(ctx, "orders-replica", "reader", "read-only", ""); status(err) != 409 {
		t.Fatalf("users are managed on the source: %v", err)
	}
	if err := m.promote(ctx, "orders"); status(err) != 409 {
		t.Fatal("only replicas are promoted")
	}
	d.fakeDocker.mu.Lock()
	d.fakeDocker.running[source.Project] = false
	d.fakeDocker.mu.Unlock()
	if err := m.createReplica(ctx, "orders", replicaRequest{Name: "orders-two", Port: 15435}); status(err) != 409 {
		t.Fatalf("a stopped source cannot be copied: %v", err)
	}
}

func TestCreatingAndPromotingAPostgreSQLReplica(t *testing.T) {
	d := newReplicaDocker()
	m := testManager(t, d.run)
	ctx := t.Context()
	src := runningSource(t, m, d, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, MemoryMiB: 512, Parameters: map[string]string{"work_mem": "8MB"}})
	src.Layout = currentLayout
	writeJSON(m.recordPath("orders"), src)
	if err := m.createReplica(ctx, "orders", replicaRequest{Name: "orders-replica", Port: 15433}); err != nil {
		t.Fatal(err)
	}
	src, _ = m.record("orders")
	rep, _ := m.record("orders-replica")
	if rep.ReplicaOf != "orders" || rep.PendingRestore || rep.MemoryMiB != 512 || rep.Parameters["work_mem"] != "8MB" || rep.Password != src.Password {
		t.Fatalf("the replica has the source's size, settings and owner: %+v", rep)
	}
	if len(src.ReplicationPassword) != 32 {
		t.Fatalf("MySQL allows replication passwords of up to 32 characters: %q", src.ReplicationPassword)
	}
	prep := d.input("psql")
	for _, want := range []string{"CREATE ROLE openrds_replication REPLICATION LOGIN", "ALTER ROLE openrds_replication WITH REPLICATION LOGIN PASSWORD 'SCRAM-SHA-256$",
		"pg_create_physical_replication_slot('openrds_orders_replica', true)", "max_slot_wal_keep_size = '16GB'"} {
		if !strings.Contains(prep, want) {
			t.Errorf("the source is not prepared with %q:\n%s", want, prep)
		}
	}
	if !strings.Contains(d.input("pg_hba.conf"), "hostssl replication openrds_replication") {
		t.Error("the running source gets the replication line in pg_hba.conf")
	}
	if got := d.input(physicalBaseScript); got != "base backup" {
		t.Fatalf("pg_basebackup streams into the replica's volume: %q", got)
	}
	standby := d.input("standby.signal")
	if !strings.Contains(standby, "host=openrds-orders port=5432 user=openrds_replication password="+src.ReplicationPassword+" sslmode=verify-full") ||
		!strings.Contains(standby, "primary_slot_name = 'openrds_orders_replica'") {
		t.Fatalf("standby settings: %s", standby)
	}
	for _, c := range d.calls {
		if strings.Contains(strings.Join(c, " "), src.ReplicationPassword) {
			t.Fatalf("the replication password reached a command line: %v", c)
		}
	}
	if !d.called("network", "connect", src.Project+"_default") {
		t.Fatal("the replica joins its source's network")
	}
	items, _ := m.readActivity()
	if items[0].Action != "Create read replica" || items[0].Result != "completed" || items[0].Detail != "of orders" {
		t.Fatalf("events: %+v", items[0])
	}
	views, _ := m.instances(ctx)
	for _, v := range views {
		if v.Name == "orders" && !slices.Equal(v.Replicas, []string{"orders-replica"}) || v.Name == "orders-replica" && (v.ReplicaOf != "orders" || v.Replication == nil || v.Replication.State != "replicating") {
			t.Errorf("view %s: replicas %v, replicaOf %q, replication %+v", v.Name, v.Replicas, v.ReplicaOf, v.Replication)
		}
	}

	if err := m.promote(ctx, "orders-replica"); err != nil {
		t.Fatal(err)
	}
	rep, _ = m.record("orders-replica")
	if rep.ReplicaOf != "" || rep.ReplicationPassword != "" || rep.Password != src.Password {
		t.Fatalf("the promoted database keeps its users and forgets its source: %+v", rep)
	}
	if all := d.everything(); !strings.Contains(all, "pg_promote(true, 300)") || !strings.Contains(all, "ALTER SYSTEM RESET primary_conninfo") ||
		!strings.Contains(all, "pg_drop_replication_slot('openrds_orders_replica')") {
		t.Fatal("promotion ends recovery, forgets the source and drops the slot")
	}
	if !d.called("network", "disconnect", src.Project+"_default") {
		t.Fatal("the promoted database leaves its source's network")
	}
	if err := m.remove(ctx, "orders", false); err != nil {
		t.Fatalf("a source without replicas can be deleted: %v", err)
	}
}

func TestValkeyReplicasGetUserChangesAndPromoteSafely(t *testing.T) {
	d := newReplicaDocker()
	d.answer = "# Replication\r\nrole:slave\r\nmaster_link_status:up\r\n"
	m := testManager(t, d.run)
	ctx := t.Context()
	src := runningSource(t, m, d, instanceSpec{Name: "cache", Engine: "valkey", Port: 16379})
	src.Layout = currentLayout
	writeJSON(m.recordPath("cache"), src)
	if err := m.createReplica(ctx, "cache", replicaRequest{Name: "cache-replica", Port: 16380}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.everything(), "ACL SETUSER openrds_replication reset on #") {
		t.Fatal("the source gets a user that can only replicate")
	}
	src, _ = m.record("cache")
	rep, _ := m.record("cache-replica")
	rep.Layout = currentLayout
	writeJSON(m.recordPath("cache-replica"), rep)
	before := len(d.inputs)
	if _, err := m.createUser(ctx, "cache", "reader", "read-only", "reader-password-123456"); err != nil {
		t.Fatal(err)
	}
	if len(d.inputs) == before {
		t.Fatal("no input recorded")
	}
	// The replica's ACL change authenticates with the owner password, then its users file is rewritten.
	got := d.input("valkey-cli")
	if strings.Count(got, "ACL SETUSER reader reset on #") < 2 {
		t.Fatalf("the new user reaches the source and the replica:\n%s", got)
	}
	file := d.input("users.conf")
	if !strings.Contains(file, "user reader on #") || !strings.Contains(file, "replicaof openrds-cache 6379") {
		t.Fatalf("the replica's users file keeps the user and its source:\n%s", file)
	}

	d.mu.Lock()
	d.inputs = map[string]string{}
	d.calls = nil
	d.mu.Unlock()
	if err := m.promote(ctx, "cache-replica"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	order := []string{}
	for _, c := range d.calls {
		if line := strings.Join(c, " "); strings.Contains(line, "users.conf") || strings.Contains(line, "valkey-cli") {
			order = append(order, line)
		}
	}
	d.mu.Unlock()
	if len(order) < 2 || !strings.Contains(order[0], "users.conf") || !strings.Contains(order[1], "valkey-cli") {
		t.Fatalf("the file loses replicaof before the server leaves its source: %v", order)
	}
	if file := d.input("users.conf"); strings.Contains(file, "replicaof") || !strings.Contains(file, "user reader on #") {
		t.Fatalf("after promotion: %s", file)
	}
	if !strings.Contains(d.input("valkey-cli"), "REPLICAOF NO ONE") {
		t.Fatal("the server stops replicating")
	}
}

func TestReplicationIsReadAndAlerted(t *testing.T) {
	d := newReplicaDocker()
	m := testManager(t, d.run)
	ctx := t.Context()
	r := record{instanceSpec: instanceSpec{Name: "orders-replica", Engine: "mysql"}, Project: "openrds-db-rep", ReplicaOf: "orders"}
	d.answer = "Replica_IO_State\tReplica_IO_Running\tReplica_SQL_Running\tLast_IO_Error\tLast_SQL_Error\tSeconds_Behind_Source\n" +
		"Waiting\tYes\tYes\t\t\t42\n"
	if st := m.replicationStatus(ctx, r, "abcdef123456"); st.State != "replicating" || st.Lag == nil || *st.Lag != 42 {
		t.Fatalf("MySQL: %+v", st)
	}
	d.answer = "Replica_IO_State\tReplica_IO_Running\tReplica_SQL_Running\tLast_IO_Error\tLast_SQL_Error\tSeconds_Behind_Source\n" +
		"\tYes\tNo\t\tCould not execute Write_rows event\\non table app.t\tNULL\n"
	if st := m.replicationStatus(ctx, r, "abcdef123456"); st.State != "broken" || st.Lag != nil || !strings.Contains(st.Detail, "Write_rows event on table") {
		t.Fatalf("MySQL broken: %+v", st)
	}
	r.Engine = "valkey"
	d.answer = "role:slave\r\nmaster_link_status:down\r\nmaster_sync_in_progress:1\r\n"
	if st := m.replicationStatus(ctx, r, "abcdef123456"); st.State != "syncing" {
		t.Fatalf("Valkey: %+v", st)
	}

	// Alerts: noted first, then a broken replica, then its recovery.
	src := runningSource(t, m, d, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432})
	rep := runningSource(t, m, d, instanceSpec{Name: "orders-replica", Engine: "postgres", Port: 15433})
	rep.ReplicaOf = src.Name
	writeJSON(m.recordPath(rep.Name), rep)
	check := func(answer string) {
		d.mu.Lock()
		d.answers = map[string]string{"pg_stat_wal_receiver": answer, "wal_status": "lost\n"}
		d.mu.Unlock()
		m.forgetCache(rep.Name)
		m.watchReplication(ctx)
	}
	check("streaming|0\n")
	check("startup|\n") // not streaming, and the source no longer keeps its WAL
	check("streaming|3\n")
	check(fmt.Sprintf("streaming|%d\n", int((lagAlert + time.Minute).Seconds())))
	items, _ := m.readActivity()
	var actions []string
	for _, e := range items {
		if e.Name == rep.Name && e.Action != "Provision database" {
			actions = append(actions, e.Action)
		}
	}
	slices.Reverse(actions)
	if !slices.Equal(actions, []string{"Replication stopped", "Replicating again", "Replica lagging"}) {
		t.Fatalf("alerts: %v", actions)
	}
}

func (m *manager) forgetCache(name string) {
	m.replication.mu.Lock()
	delete(m.replication.status, name)
	m.replication.mu.Unlock()
}
