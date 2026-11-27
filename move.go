package main

// Moving a database to another server with seconds of write downtime: a read
// replica on the target server copies the database and follows it; then
// writes pause while the replica takes the last changes, the replica is
// promoted, and it takes over the database's name and settings. The old copy
// is removed once the moved database runs. Until then, a failure puts things
// back as they were.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	// How long the replica may take to catch up before writes pause.
	moveCatchUp = 30 * time.Minute
	// The longest writes stay paused while the last changes arrive.
	moveFreeze = time.Minute
)

// Where a running move is, for the console: 1 copying, 2 catching up,
// 3 switching with writes paused, 4 cleaning up.
type moveStep struct {
	To   string `json:"to"`
	Step int    `json:"step"`
}

func (m *manager) noteMove(name, to string, step int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moving == nil {
		m.moving = map[string]moveStep{}
	}
	if step == 0 {
		delete(m.moving, name)
		return
	}
	m.moving[name] = moveStep{to, step}
}

type moveRequest struct {
	Server string `json:"server"`
	Port   int    `json:"port"` // the database's own when empty and free there
}

// The temporary replica's name, unique and within the 40 characters.
func moveName(name string) string {
	return fmt.Sprintf("%s-mv-%s", strings.TrimRight(name[:min(len(name), 31)], "-"), randomHex(2))
}

// What the database becomes on the target server: its own settings, the
// target's ports, and private access only where the target has a private
// address.
func (m *manager) movedSpec(all []record, d record, target string, port int) (instanceSpec, error) {
	s := d.instanceSpec
	from, _ := m.server(d.Server)
	to, _ := m.server(target)
	s.Server, s.Port = target, port
	if s.Server == localServer {
		s.Server = ""
	}
	if s.PublicHost != "" && s.PublicHost == from.PublicAddress {
		s.PublicHost = "" // the target's public address takes its place
	}
	if to.PrivateAddress == "" {
		s.PrivateAccess = false
	}
	others := make([]record, 0, len(all))
	for _, r := range all {
		if r.Name != d.Name {
			others = append(others, r)
		}
	}
	// Public and pooler ports stay when they are free on the target.
	taken := func(port int) bool {
		return checkPorts(others, instanceSpec{Name: d.Name, Server: s.Server, Port: port}) != nil
	}
	if s.PublicAccess && taken(s.PublicPort) {
		s.PublicPort = suggestPublicPort(others, s)
	}
	if s.Pooler.on() {
		pooler := *s.Pooler
		s.Pooler = &pooler
		if taken(pooler.Port) {
			s.Pooler.Port = 0
			normalizePooler(&s, others)
		}
	}
	if err := checkPorts(others, s); err != nil {
		return s, err
	}
	return s, nil
}

func (m *manager) move(ctx context.Context, name string, req moveRequest) (err error) {
	d, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	target := req.Server
	if target == "" || target == localServer {
		target = ""
	}
	from, _ := m.server(d.Server)
	to, ok := m.server(target)
	all, err := m.records()
	if err != nil {
		return err
	}
	states, err := m.states(ctx)
	if err != nil {
		return fail(503, "Cannot read database status: "+err.Error())
	}
	switch {
	case !ok:
		return fail(400, "Choose a server from the Servers page.")
	case sameServer(d.Server, target):
		return fail(400, fmt.Sprintf("%s already runs on %s.", d.Name, to.Name))
	case target != "" && m.recentlyDown(target):
		return fail(409, to.Name+" is unreachable.")
	case d.ReplicaOf != "":
		return fail(409, fmt.Sprintf("%s is a read replica. Create a replica of %s on %s instead.", d.Name, d.ReplicaOf, to.Name))
	case len(replicasOf(all, d.Name)) > 0:
		return fail(409, fmt.Sprintf("%s has read replicas: %s. Promote or delete them first.", d.Name, replicaNames(replicasOf(all, d.Name))))
	case d.PendingRestore:
		return fail(409, "This instance has an incomplete restore.")
	case states[d.Project] != "running":
		return fail(409, fmt.Sprintf("Start %s and wait until it is healthy: it is copied while it runs.", d.Name))
	}
	port := req.Port
	if port == 0 {
		port = d.Port
	}
	spec, err := m.movedSpec(all, d, target, port)
	if err != nil {
		return err
	}
	finish := m.trackDetail(ctx, d.Name, "Move database", fmt.Sprintf("from %s to %s", from.Name, to.Name))
	defer func() { finish(err) }()
	m.noteMove(d.Name, to.Name, 1)
	defer m.noteMove(d.Name, "", 0)

	// 1. A replica on the target server, caught up.
	temp := moveName(d.Name)
	rep, err := m.replicate(ctx, d, replicaRequest{Name: temp, Port: port, Server: serverOrLocal(target)})
	discard := func(cause error) error {
		var p *problem
		if err := m.remove(context.WithoutCancel(ctx), temp, false); err != nil && !(errors.As(err, &p) && p.status == 404) {
			log.Printf("removing the copy of %s on %s: %v", d.Name, to.Name, err)
		}
		return cause
	}
	if err != nil {
		return discard(fail(502, fmt.Sprintf("%s was not moved, and keeps running on %s: the copy on %s failed. %s", d.Name, from.Name, to.Name, describe(err))))
	}
	m.noteMove(d.Name, to.Name, 2)
	if err := m.awaitCaughtUp(ctx, rep); err != nil {
		return discard(fail(502, fmt.Sprintf("%s was not moved, and keeps running on %s: %s", d.Name, from.Name, err)))
	}

	// 2. Writes pause; the last changes arrive; the copy takes over.
	thaw := func() {
		if err := m.thawWrites(context.WithoutCancel(ctx), d); err != nil {
			log.Printf("letting writes back into %s: %v", d.Name, err)
		}
	}
	m.noteMove(d.Name, to.Name, 3)
	if err := m.freezeWrites(ctx, d); err != nil {
		thaw()
		return discard(fail(502, fmt.Sprintf("%s was not moved: writes could not be paused. %s", d.Name, err)))
	}
	if err := m.awaitLastChanges(ctx, d, rep); err != nil {
		thaw()
		return discard(fail(502, fmt.Sprintf("%s was not moved, and takes writes again on %s: %s", d.Name, from.Name, err)))
	}
	promoted, err := m.promoteRecord(ctx, rep)
	if err != nil {
		thaw()
		return discard(fail(502, fmt.Sprintf("%s was not moved, and takes writes again on %s. %s", d.Name, from.Name, describe(err))))
	}
	if err := m.stop(ctx, d); err != nil {
		thaw()
		return discard(fail(502, fmt.Sprintf("%s was not moved, and takes writes again on %s: it could not be stopped. %s", d.Name, from.Name, err)))
	}

	// 3. The copy becomes the database: its name, settings and history.
	moved := promoted
	moved.instanceSpec = spec
	moved.DiskGiB = promoted.DiskGiB
	moved.Created, moved.Users, moved.Password = d.Created, d.Users, d.Password
	moved.ReplicationPassword, moved.Applied, moved.PendingRestore = "", nil, false
	if err := writeJSON(m.recordPath(d.Name), moved); err != nil {
		return m.moveBack(ctx, d, moved, fmt.Errorf("saving it: %w", err))
	}
	os.Remove(m.recordPath(temp))
	m.forgetReplication(rep)
	if err := m.start(ctx, &moved); err != nil {
		return m.moveBack(ctx, d, moved, err)
	}

	// 4. The old copy goes.
	m.noteMove(d.Name, to.Name, 4)
	if err := m.dropCopy(context.WithoutCancel(ctx), d); err != nil {
		m.note(ctx, d.Name, "Remove the copy left by a move", "failed", fmt.Sprintf("The old copy on %s is still there: %s", from.Name, err))
	}
	return nil
}

func serverOrLocal(server string) string {
	if server == "" {
		return localServer
	}
	return server
}

// Undo a move after the copy was promoted but did not start as the
// database: the original runs and takes writes again, the copy goes.
func (m *manager) moveBack(ctx context.Context, d, moved record, cause error) error {
	ctx = context.WithoutCancel(ctx)
	m.compose(ctx, moved, nil, nil, "down", "--volumes", "--remove-orphans", "--timeout", "45")
	if moved.DiskGiB > 0 {
		m.detachDisk(ctx, moved)
	}
	m.forgetTLS(moved)
	os.Remove(m.overridePath(moved))
	if err := writeJSON(m.recordPath(d.Name), d); err != nil {
		return fail(502, "The move failed and the database’s settings could not be put back: "+err.Error())
	}
	if err := m.start(ctx, &d); err != nil {
		return fail(502, fmt.Sprintf("The move failed, and %s did not start again on its server. Its data is intact; start it from the console. %s", d.Name, cause))
	}
	if err := m.thawWrites(ctx, d); err != nil {
		log.Printf("letting writes back into %s: %v", d.Name, err)
	}
	return fail(502, fmt.Sprintf("%s was not moved, and runs on its server again. %s", d.Name, cause))
}

// Remove what is left of a database on its old server.
func (m *manager) dropCopy(ctx context.Context, d record) error {
	if err := m.compose(ctx, d, nil, nil, "down", "--volumes", "--remove-orphans", "--timeout", "45"); err != nil {
		return err
	}
	if d.DiskGiB > 0 {
		if err := m.detachDisk(ctx, d); err != nil {
			return err
		}
	}
	m.on(d).run(ctx, nil, nil, nil, "volume", "rm", "--force", d.Project+"_archive")
	m.history.forget(d.Project)
	m.forgetTLS(d)
	if err := os.Remove(m.overridePath(d)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---------- pausing writes

// Writes pause, but replication keeps going, unlike for an upgrade:
// PostgreSQL makes new transactions read-only and ends client sessions so
// they reconnect read-only, MySQL turns on super_read_only, and Valkey pauses
// clients' writes.
func (m *manager) freezeWrites(ctx context.Context, r record) error {
	switch r.Engine {
	case "postgres":
		return m.admin(ctx, r, `ALTER SYSTEM SET default_transaction_read_only = on;
SELECT pg_reload_conf();
SELECT pg_sleep(0.5);
SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid();
`)
	case "mysql":
		return m.admin(ctx, r, "SET GLOBAL super_read_only = ON;\n")
	default:
		return m.admin(ctx, r, "CLIENT PAUSE "+strconv.Itoa(int(moveFreeze/time.Millisecond)*5)+" WRITE\n")
	}
}

func (m *manager) thawWrites(ctx context.Context, r record) error {
	switch r.Engine {
	case "postgres":
		return m.admin(ctx, r, "ALTER SYSTEM RESET default_transaction_read_only;\nSELECT pg_reload_conf();\n")
	case "mysql":
		return m.admin(ctx, r, "SET GLOBAL super_read_only = OFF;\nSET GLOBAL read_only = OFF;\n")
	default:
		return m.admin(ctx, r, "CLIENT UNPAUSE\n")
	}
}

// ---------- catching up

// Wait until the replica follows with little lag, before writes pause.
func (m *manager) awaitCaughtUp(ctx context.Context, rep record) error {
	deadline := time.Now().Add(moveCatchUp)
	for {
		id, err := m.runningContainer(ctx, rep)
		if err == nil {
			st := m.replicationStatus(ctx, rep, id)
			if st.State == "replicating" && (st.Lag == nil || *st.Lag <= 5) {
				return nil
			}
			if st.State == "broken" {
				return fmt.Errorf("the copy stopped following it. %s", st.Detail)
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the copy did not catch up within 30 minutes")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Wait until the replica has every change the paused database wrote.
func (m *manager) awaitLastChanges(ctx context.Context, d, rep record) error {
	deadline := time.Now().Add(moveFreeze)
	for {
		done, err := m.caughtUp(ctx, d, rep)
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("the copy did not take the last changes within a minute: %v", err)
			}
			return errors.New("the copy did not take the last changes within a minute")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Whether the replica has applied everything its source wrote.
func (m *manager) caughtUp(ctx context.Context, src, rep record) (bool, error) {
	srcID, err := m.runningContainer(ctx, src)
	if err != nil {
		return false, err
	}
	repID, err := m.runningContainer(ctx, rep)
	if err != nil {
		return false, err
	}
	switch src.Engine {
	case "postgres":
		wrote, err1 := m.engineClient(ctx, src, srcID, pgClient, "SELECT /* openrds */ pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0');\n")
		applied, err2 := m.engineClient(ctx, rep, repID, pgClient, "SELECT /* openrds */ pg_wal_lsn_diff(pg_last_wal_replay_lsn(), '0/0');\n")
		if err := errors.Join(err1, err2); err != nil {
			return false, err
		}
		w, err1 := strconv.ParseFloat(strings.TrimSpace(wrote), 64)
		a, err2 := strconv.ParseFloat(strings.TrimSpace(applied), 64)
		return err1 == nil && err2 == nil && a >= w, errors.Join(err1, err2)
	case "mysql":
		wrote, err1 := m.engineClient(ctx, src, srcID, mysqlStatusClient, "SHOW BINARY LOG STATUS;\n")
		applied, err2 := m.engineClient(ctx, rep, repID, mysqlStatusClient, "SHOW REPLICA STATUS;\n")
		if err := errors.Join(err1, err2); err != nil {
			return false, err
		}
		s, ok1 := replicaFields(wrote)
		r, ok2 := replicaFields(applied)
		if !ok1 || !ok2 {
			return false, errors.New("no binary log position")
		}
		at, _ := strconv.ParseInt(s["Position"], 10, 64)
		done, _ := strconv.ParseInt(r["Exec_Source_Log_Pos"], 10, 64)
		return r["Relay_Source_Log_File"] == s["File"] && done >= at, nil
	default:
		wrote, err1 := m.engineClient(ctx, src, srcID, valkeyClient, src.Password+"\nINFO replication\n")
		applied, err2 := m.engineClient(ctx, rep, repID, valkeyClient, rep.Password+"\nINFO replication\n")
		if err := errors.Join(err1, err2); err != nil {
			return false, err
		}
		w, a := infoField(wrote, "master_repl_offset"), infoField(applied, "master_repl_offset")
		return w != "" && w == a, nil
	}
}

func infoField(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == key {
			return v
		}
	}
	return ""
}

func moveRoutes(add func(string, int, endpoint), m *manager) {
	add("POST /api/instances/{name}/move", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var req moveRequest
		if err := decode(w, r, &req); err != nil {
			return err
		}
		if err := m.move(r.Context(), r.PathValue("name"), req); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
