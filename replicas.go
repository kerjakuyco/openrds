package main

// Read replicas, like RDS read replicas: a database that follows another
// one's changes and serves reads. A replica is an instance of its own, with
// its own port, size, pooler, public access and backups. PostgreSQL streams
// WAL through a replication slot, MySQL applies the binary log from the
// position of a consistent dump, and Valkey uses its own replication.
// Replicas have their source's users and passwords. Promoting one makes it a
// standalone, writable database.
//
// A replica on its source's server reaches the source on the source's Docker
// network. OpenRDS attaches the replica's container to that network itself,
// without aliases: Compose would give it the service name "db" there, and the
// source's pooler and gateway find the source by that name. A replica on
// another server connects over the private network when both servers have a
// private address, otherwise over the internet through the source's public
// access; the source's gateway lets in only the replica's server.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxReplicas     = 5
	replicationUser = "openrds_replication"
	// How long a new replica may take to start following its source.
	followWait = 3 * time.Minute
	// Lag that is reported, and when it counts as caught up again.
	lagAlert   = 10 * time.Minute
	lagCleared = time.Minute
	// How long a replica may fail to reach its running source before an alert.
	unreachableAlert = 10 * time.Minute
)

// What a replica is doing, as last read from the engine.
type replicationStatus struct {
	State  string    `json:"state"` // replicating, syncing, connecting, broken or stopped
	Lag    *float64  `json:"lagSeconds,omitempty"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

type replicationState struct {
	watching sync.Mutex // one check at a time
	mu       sync.Mutex
	status   map[string]replicationStatus // by replica name
	marks    map[string]replicaMark       // what the last check alerted on, by Compose project
}

type replicaMark struct {
	state       string
	lagging     bool
	unreachable time.Time // since when the replica has been connecting
	alerted     bool      // an unreachable alert was sent
}

// A replica has its source's users and passwords: PostgreSQL and MySQL copy
// them with the data, and OpenRDS applies Valkey's to both.
func (r *record) follow(src record) {
	r.Password, r.Users, r.ReplicationPassword, r.sourceTLS = src.Password, src.Users, src.ReplicationPassword, src.RequireTLS
}

func replicasOf(all []record, source string) []record {
	var out []record
	for _, r := range all {
		if r.ReplicaOf == source {
			out = append(out, r)
		}
	}
	return out
}

func replicaNames(list []record) string {
	names := make([]string, len(list))
	for i, r := range list {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

func (m *manager) replicasOf(source string) ([]record, error) {
	all, err := m.records()
	if err != nil {
		return nil, err
	}
	return replicasOf(all, source), nil
}

// The physical replication slot that keeps a PostgreSQL replica's WAL on
// its source until the replica has it.
func slotName(r record) string { return "openrds_" + strings.ReplaceAll(r.Name, "-", "_") }

// How much WAL slots may keep on a source, so a replica that stays away
// cannot fill the disk: a quarter of a disk limit, or 16GB. A replica that
// falls further behind stops and has to be created again.
func slotKeep(r record) string {
	if r.DiskGiB > 0 {
		return pgSize(max(r.DiskGiB*1024/4, 1024))
	}
	return "16GB"
}

// Where a replica reaches its source: by the source's network name on the
// same server, otherwise at the address its route gave it.
func (r record) sourceEndpoint() (string, int) {
	if host, port, err := net.SplitHostPort(r.SourceAddress); err == nil {
		n, _ := strconv.Atoi(port)
		return host, n
	}
	return "openrds-" + r.ReplicaOf, engines[r.Engine].port
}

func pgConninfo(r record) string {
	host, port := r.sourceEndpoint()
	return fmt.Sprintf("host=%s port=%d user=%s password=%s sslmode=verify-full sslrootcert=%s application_name=%s",
		host, port, replicationUser, r.ReplicationPassword, tlsCAFile, r.Name)
}

// A MySQL server ID other than 1, MySQL's default, which sources keep.
func serverID() int {
	var b [4]byte
	rand.Read(b[:])
	return int(binary.BigEndian.Uint32(b[:])%(1<<31-2)) + 2
}

// The replica's lines in its Valkey configuration file, next to its users.
func valkeyReplicaConfig(r record) string {
	tls := "no"
	if r.sourceTLS {
		tls = "yes"
	}
	host, port := r.sourceEndpoint()
	return fmt.Sprintf(`replicaof %s %d
masteruser %s
masterauth %s
tls-cert-file %s
tls-key-file %s
tls-ca-cert-file %s
tls-replication %s
`, host, port, replicationUser, r.ReplicationPassword, tlsCertFile, tlsKeyFile, tlsCAFile, tls)
}

// Remove passwords from an engine's error text: MySQL quotes the statement.
func redact(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "[redacted]")
		}
	}
	return errors.New(text)
}

// The PostgreSQL settings a standby needs at least as high as its primary's,
// among those users can set. It refuses to start otherwise.
var standbyMinimums = []struct{ name, fallback string }{{"max_connections", "100"}, {"max_worker_processes", "8"}}

func checkStandby(src, replica record) error {
	if src.Engine != "postgres" {
		return nil
	}
	value := func(r record, name, fallback string) int {
		v := r.effectiveParameters()[name]
		if v == "" {
			v = fallback
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	for _, p := range standbyMinimums {
		if have, need := value(replica, p.name, p.fallback), value(src, p.name, p.fallback); have < need {
			return fail(400, fmt.Sprintf("Read replica %s needs %s of at least %d, as on %s. Raise it on the replica first.", replica.Name, p.name, need, src.Name))
		}
	}
	return nil
}

// Replication rules for a modification: replicas do not collect logs, and
// PostgreSQL standbys keep some settings at least as high as their source.
func checkReplication(all []record, next record) error {
	if next.ReplicaOf != "" {
		if next.PITR {
			return fail(400, fmt.Sprintf("Read replicas don’t have point-in-time recovery. Turn it on for %s, their source.", next.ReplicaOf))
		}
		for _, src := range all {
			if src.Name == next.ReplicaOf {
				return checkStandby(src, next)
			}
		}
		return nil
	}
	var current record
	for _, r := range all {
		if r.Name == next.Name {
			current = r
		}
	}
	for _, r := range replicasOf(all, next.Name) {
		if err := checkStandby(next, r); err != nil {
			return err
		}
		switch {
		case r.SourceRoute == routePrivate && next.Port != current.Port:
			return fail(409, fmt.Sprintf("Read replica %s on another server connects to port %d. Promote or delete it before changing the port.", r.Name, current.Port))
		case r.SourceRoute == routePublic && (!next.PublicAccess || next.PublicPort != current.PublicPort || next.PublicHost != current.PublicHost):
			return fail(409, fmt.Sprintf("Read replica %s on another server connects through public access at %s. Promote or delete it before changing public access.", r.Name, r.SourceAddress))
		}
	}
	return nil
}

const (
	routePrivate = "private" // over the private network, to the source's private address
	routePublic  = "public"  // over the internet, through the source's public access
)

// How a replica on another server reaches its source: over the private
// network when both servers have a private address, otherwise through the
// source's public access. A replica on the source's server needs no route.
func (m *manager) replicaRoute(src record, server string) (address, route string, err error) {
	if sameServer(src.Server, server) {
		return "", "", nil
	}
	from, ok := m.server(src.Server)
	to, ok2 := m.server(server)
	if !ok || !ok2 {
		return "", "", fail(400, "Choose a server from the Servers page.")
	}
	if from.PrivateAddress != "" && to.PrivateAddress != "" {
		return net.JoinHostPort(from.PrivateAddress, strconv.Itoa(src.Port)), routePrivate, nil
	}
	host := cmp.Or(src.PublicHost, from.PublicAddress)
	switch {
	case !src.PublicAccess:
		return "", "", fail(409, fmt.Sprintf("%s and %s share no private network. Give both a private address on the Servers page, or turn on public access for %s so the replica connects over the internet.", from.Name, to.Name, src.Name))
	case host == "":
		return "", "", fail(409, fmt.Sprintf("Set the public address of %s on the Servers page: the replica connects to it.", from.Name))
	case to.PublicAddress == "":
		return "", "", fail(409, fmt.Sprintf("Set the public address of %s on the Servers page: %s lets in replication from it.", to.Name, src.Name))
	}
	return net.JoinHostPort(host, strconv.Itoa(src.PublicPort)), routePublic, nil
}

// The sources a database's gateways let in for its replicas on other
// servers, besides the rules people set.
func (m *manager) replicaSources(r record) (private, public []string) {
	if r.ReplicaOf != "" || r.ReplicationPassword == "" {
		return nil, nil
	}
	all, err := m.records()
	if err != nil {
		return nil, nil
	}
	for _, rep := range replicasOf(all, r.Name) {
		srv, ok := m.server(rep.Server)
		if !ok {
			continue
		}
		switch rep.SourceRoute {
		case routePrivate:
			if srv.PrivateAddress != "" {
				private = append(private, srv.PrivateAddress+"/32")
			}
		case routePublic:
			public = append(public, addressRanges(srv.PublicAddress)...)
		}
	}
	return private, public
}

// A server's public address as ranges for an allowlist: an IP, or what a
// DNS name resolves to.
func addressRanges(address string) []string {
	ips := []string{address}
	if net.ParseIP(address) == nil {
		resolved, err := net.LookupHost(address)
		if err != nil {
			log.Printf("resolving %s for replication: %v", address, err)
		}
		ips = resolved
	}
	var out []string
	for _, ip := range ips {
		if p, err := parseSource(ip); err == nil {
			out = append(out, p.String())
		}
	}
	return out
}

// Whether a database's private gateway runs: for private access, or for its
// replicas on other servers.
func (m *manager) servesPrivately(r record) bool {
	srv, ok := m.server(r.Server)
	if !ok || srv.PrivateAddress == "" {
		return false
	}
	private, _ := m.replicaSources(r)
	return r.PrivateAccess || len(private) > 0
}

// Open or close the source's gateways for its replicas on other servers.
func (m *manager) applySourceAccess(ctx context.Context, src record) error {
	states, err := m.states(ctx)
	if err != nil {
		return err
	}
	live := live(states[src.Project])
	if err := m.applyPrivateGateway(ctx, src, src, live); err != nil {
		return err
	}
	if src.PublicAccess && live {
		return m.compose(ctx, src, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "60", "gateway")
	}
	return nil
}

// The names a running database's certificate covers, read from its container.
func (m *manager) servedNames(ctx context.Context, r record) ([]string, error) {
	var out bytes.Buffer
	if err := m.compose(ctx, r, nil, &out, "exec", "-T", "db", "cat", tlsCertFile); err != nil {
		return nil, err
	}
	block, _ := pem.Decode(out.Bytes())
	if block == nil {
		return nil, errors.New("no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return certHosts(cert), nil
}

type replicaRequest struct {
	Name      string   `json:"name"`
	Port      int      `json:"port"`
	Server    string   `json:"server"` // the source's when empty
	CPUs      *float64 `json:"cpus"`
	MemoryMiB *int     `json:"memoryMiB"`
	DiskGiB   *int     `json:"diskGiB"`
}

// Create a read replica of source. The source stays available; it is held
// for other operations while its data is copied, so the copy and the users
// the replica starts with match.
func (m *manager) createReplica(ctx context.Context, source string, req replicaRequest) (err error) {
	src, releaseSource, err := m.begin(source, nil, false)
	if err != nil {
		return err
	}
	defer releaseSource()
	all, err := m.records()
	if err != nil {
		return err
	}
	states, err := m.states(ctx)
	if err != nil {
		return fail(503, "Cannot read database status: "+err.Error())
	}
	switch reps := replicasOf(all, src.Name); {
	case src.ReplicaOf != "":
		return fail(409, fmt.Sprintf("%s is a read replica. Create replicas of its source, %s.", src.Name, src.ReplicaOf))
	case src.PendingRestore:
		return fail(409, "This instance has an incomplete restore.")
	case states[src.Project] != "running":
		return fail(409, fmt.Sprintf("Start %s and wait until it is healthy: the replica copies its data while it runs.", src.Name))
	case src.Layout < currentLayout:
		return fail(409, fmt.Sprintf("Reboot %s once first. It was started by an earlier version of OpenRDS.", src.Name))
	case len(reps) >= maxReplicas:
		return fail(409, fmt.Sprintf("%s already has %d read replicas, the most it can have.", src.Name, maxReplicas))
	}
	spec := instanceSpec{Name: req.Name, Engine: src.Engine, Version: src.version(), Server: src.Server, Port: req.Port, CPUs: src.CPUs, MemoryMiB: src.MemoryMiB,
		DiskGiB: src.DiskGiB, Parameters: maps.Clone(src.Parameters), RequireTLS: src.RequireTLS, BackupWindow: src.BackupWindow, Tags: maps.Clone(src.Tags)}
	if req.CPUs != nil {
		spec.CPUs = *req.CPUs
	}
	if req.MemoryMiB != nil {
		spec.MemoryMiB = *req.MemoryMiB
	}
	if req.DiskGiB != nil {
		spec.DiskGiB = *req.DiskGiB
	}
	if req.Server != "" {
		spec.Server = req.Server
		if spec.Server == localServer {
			spec.Server = ""
		}
	}
	address, route, err := m.replicaRoute(src, spec.Server)
	if err != nil {
		return err
	}
	if route != "" {
		// The replica checks the source's certificate against the address it dials.
		host, _, _ := net.SplitHostPort(address)
		names, err := m.servedNames(ctx, src)
		if err != nil {
			return fail(502, fmt.Sprintf("Could not read the certificate %s uses: %s", src.Name, err))
		}
		if !slices.Contains(names, host) {
			return fail(409, fmt.Sprintf("Reboot %s once first, so its certificate names %s, the address the replica connects to.", src.Name, host))
		}
	}
	if src.DiskGiB > 0 && spec.DiskGiB > 0 && spec.DiskGiB < src.DiskGiB {
		return fail(400, fmt.Sprintf("Give the replica at least the %d GiB of storage %s has: it holds the same data.", src.DiskGiB, src.Name))
	}
	if err := checkStandby(src, record{instanceSpec: spec}); err != nil {
		return err
	}
	if err := m.checkDiskRequest(ctx, spec.Server, spec.DiskGiB); err != nil {
		return err
	}
	if src.ReplicationPassword == "" {
		src.ReplicationPassword = randomHex(16) // MySQL allows 32 characters
		if err := m.save(src); err != nil {
			return err
		}
	}
	r, release, err := m.begin(spec.Name, &spec, true)
	if err != nil {
		return err
	}
	defer release()
	r.ReplicaOf, r.SourceAddress, r.SourceRoute = src.Name, address, route
	r.follow(src)
	if r.Engine == "mysql" {
		r.ServerID = serverID()
	}
	if err := writeJSON(m.recordPath(r.Name), r); err != nil {
		return err
	}
	finish := m.trackDetail(ctx, r.Name, "Create read replica", "of "+src.Name)
	defer func() { finish(err) }()
	if err := m.prepareSource(ctx, src, r); err != nil {
		os.Remove(m.recordPath(r.Name)) // nothing else exists yet
		return fail(502, fmt.Sprintf("The replica was not created: %s could not be prepared for replication. %s", src.Name, redact(err, src.ReplicationPassword)))
	}
	if route != "" {
		if err := m.applySourceAccess(ctx, src); err != nil {
			os.Remove(m.recordPath(r.Name))
			m.applySourceAccess(context.WithoutCancel(ctx), src)
			return fail(502, fmt.Sprintf("The replica was not created: %s could not let in replication from its server. %s", src.Name, err))
		}
	}
	if r.DiskGiB > 0 {
		if err := m.attachDisk(ctx, r); err != nil {
			return fail(502, "The replica was not created: its disk could not be prepared. "+err.Error())
		}
	}
	switch r.Engine {
	case "postgres":
		err = m.seedPostgres(ctx, src, &r)
	case "mysql":
		err = m.seedMySQL(ctx, src, &r)
	default:
		err = m.start(ctx, &r) // Valkey copies the data itself once it connects
	}
	if err != nil {
		return fail(502, "The replica did not finish and is marked incomplete; delete it and try again. "+redact(err, r.ReplicationPassword).Error())
	}
	r.PendingRestore = false
	if err := writeJSON(m.recordPath(r.Name), r); err != nil {
		return err
	}
	st := m.awaitReplication(ctx, r)
	if st.State != "replicating" && st.State != "syncing" {
		detail := st.Detail
		if detail == "" {
			detail = "Its logs show why."
		}
		return fail(502, fmt.Sprintf("%s is running but does not receive changes from %s yet (%s). %s", r.Name, src.Name, st.State, detail))
	}
	return nil
}

// Let replicas connect: a replication user (PostgreSQL also gets a slot for
// the new replica, and a limit on the WAL slots keep).
func (m *manager) prepareSource(ctx context.Context, src, replica record) error {
	switch src.Engine {
	case "postgres":
		if err := m.reloadHBA(ctx, src); err != nil {
			return err
		}
		return m.admin(ctx, src, fmt.Sprintf(`DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
    CREATE ROLE %[1]s REPLICATION LOGIN;
  END IF;
  IF current_setting('shared_preload_libraries') LIKE '%%pg_stat_statements%%' THEN
    CREATE EXTENSION IF NOT EXISTS pg_stat_statements; -- so replicas can show their statements
  END IF;
END $$;
ALTER ROLE %[1]s WITH REPLICATION LOGIN PASSWORD '%[2]s';
ALTER SYSTEM SET max_slot_wal_keep_size = '%[3]s';
SELECT pg_reload_conf();
SELECT pg_create_physical_replication_slot('%[4]s', true) WHERE NOT EXISTS (SELECT FROM pg_replication_slots WHERE slot_name = '%[4]s');
`, replicationUser, pgPassword(src.ReplicationPassword), slotKeep(src), slotName(replica)))
	case "mysql":
		out, err := m.query(ctx, src, "SELECT @@log_bin;\n")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "1" {
			return errors.New("its binary log is off, and replicas read changes from it")
		}
		return m.admin(ctx, src, fmt.Sprintf(`SET sql_log_bin = 0;
CREATE USER IF NOT EXISTS '%[1]s'@'%%' IDENTIFIED BY '%[2]s' REQUIRE SSL;
ALTER USER '%[1]s'@'%%' IDENTIFIED BY '%[2]s' REQUIRE SSL;
GRANT REPLICATION SLAVE ON *.* TO '%[1]s'@'%%';
`, replicationUser, src.ReplicationPassword))
	default:
		if err := m.admin(ctx, src, fmt.Sprintf("ACL SETUSER %s reset on #%s +psync +replconf +ping\n", replicationUser, valkeyHash(src.ReplicationPassword))); err != nil {
			return err
		}
		return m.writeValkeyUsers(ctx, src)
	}
}

// Write the managed pg_hba.conf into a running PostgreSQL and reload it.
func (m *manager) reloadHBA(ctx context.Context, r record) error {
	script := `cat > /run/openrds/pg_hba.conf
psql -X -q -v ON_ERROR_STOP=1 -U openrds -d app -c "SELECT pg_reload_conf()"`
	return m.compose(ctx, r, strings.NewReader(pgHBA(r)), io.Discard, "exec", "-T", "db", "sh", "-ec", script)
}

// Configuration a PostgreSQL standby reads from its data directory. The
// connection string travels on stdin, so its password stays off command lines.
const standbyScript = `cat >> "$PGDATA/postgresql.auto.conf"
touch "$PGDATA/standby.signal"
chown -R postgres:postgres "$PGDATA" && chmod 700 "$PGDATA"`

// Copy the source with pg_basebackup straight into the replica's empty
// volume, then start it as a standby on the source's slot.
func (m *manager) seedPostgres(ctx context.Context, src record, r *record) error {
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := m.compose(ctx, src, nil, pw, append([]string{"exec", "-T", "db"}, baseBackupCommand...)...)
		pw.CloseWithError(err)
		copied <- err
	}()
	err := m.compose(ctx, *r, pr, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec", physicalBaseScript)
	pr.CloseWithError(errors.New("the copy ended"))
	if err := copyFailed(src, <-copied, err); err != nil {
		return err
	}
	conf := fmt.Sprintf("primary_conninfo = '%s'\nprimary_slot_name = '%s'\n", pgConninfo(*r), slotName(*r))
	if err := m.compose(ctx, *r, strings.NewReader(conf), nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec", standbyScript); err != nil {
		return err
	}
	return m.start(ctx, r)
}

const mysqlSeedDump = `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysqldump -u root --single-transaction --quick --no-tablespaces --source-data=1 --set-gtid-purged=OFF --routines --events --triggers --hex-blob app`

// Start an empty MySQL with the source's users, point it at the source, load
// a dump that records its binary log position, then replicate from there.
// The replica stays read-only, even for administrators.
func (m *manager) seedMySQL(ctx context.Context, src record, r *record) error {
	if err := m.start(ctx, r); err != nil {
		return err
	}
	var setup strings.Builder
	setup.WriteString("SET sql_log_bin = 0;\n")
	for _, u := range r.Users {
		setup.WriteString(grantScript(*r, u))
	}
	// The host comes first: naming it resets the position, which the dump sets.
	host, port := r.sourceEndpoint()
	fmt.Fprintf(&setup, `CHANGE REPLICATION SOURCE TO SOURCE_HOST = '%s', SOURCE_PORT = %d, SOURCE_USER = '%s', SOURCE_PASSWORD = '%s',
  SOURCE_SSL = 1, SOURCE_SSL_CA = '%s', SOURCE_SSL_VERIFY_SERVER_CERT = 1, SOURCE_CONNECT_RETRY = 10, SOURCE_RETRY_COUNT = 31536000;
`, host, port, replicationUser, r.ReplicationPassword, tlsCAFile)
	if err := m.admin(ctx, *r, setup.String()); err != nil {
		return redact(err, r.ReplicationPassword)
	}
	pr, pw := io.Pipe()
	dumped := make(chan error, 1)
	go func() {
		err := m.compose(ctx, src, nil, pw, "exec", "-T", "db", "sh", "-ec", mysqlSeedDump)
		pw.CloseWithError(err)
		dumped <- err
	}()
	input := io.MultiReader(strings.NewReader("SET sql_log_bin = 0;\n"), pr)
	err := m.compose(ctx, *r, input, nil, append([]string{"exec", "-T", "db"}, engines["mysql"].restore...)...)
	pr.CloseWithError(errors.New("the load ended"))
	if err := copyFailed(src, <-dumped, err); err != nil {
		return err
	}
	return m.admin(ctx, *r, "START REPLICA;\nSET PERSIST super_read_only = ON;\n")
}

// Why copying a source into a replica failed: either side can end the copy,
// and the other then only sees it cut short, so both are reported.
func copyFailed(src record, read, write error) error {
	switch {
	case read != nil && write != nil:
		return fmt.Errorf("copying the data of %s failed: %v; the replica reported: %v", src.Name, read, write)
	case read != nil:
		return fmt.Errorf("copying the data of %s failed: %w", src.Name, read)
	case write != nil:
		return fmt.Errorf("writing the copy failed: %w", write)
	}
	return nil
}

// Attach a replica's container to its source's network, under no name of
// its own there. Docker keeps the attachment across restarts; a recreated
// container is attached again before it starts.
func (m *manager) linkReplica(ctx context.Context, r record) error {
	src, err := m.record(r.ReplicaOf)
	if err != nil {
		return err
	}
	id, err := m.containerID(ctx, r)
	if err != nil {
		return err
	}
	network := src.Project + "_default"
	var out bytes.Buffer
	if err := m.on(r).run(ctx, nil, nil, &out, "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", id); err != nil {
		return err
	}
	if slices.Contains(strings.Fields(out.String()), network) {
		return nil
	}
	if err := m.on(r).run(ctx, nil, nil, io.Discard, "network", "connect", network, id); err != nil {
		return fmt.Errorf("joining the network of %s: %w", src.Name, err)
	}
	return nil
}

func (m *manager) unlinkReplica(ctx context.Context, r record, source string) {
	src, err := m.record(source)
	if err != nil {
		return
	}
	if id, err := m.containerID(ctx, r); err == nil {
		m.on(r).run(ctx, nil, nil, io.Discard, "network", "disconnect", src.Project+"_default", id)
	}
}

// The database container, running or not.
func (m *manager) containerID(ctx context.Context, r record) (string, error) {
	var out bytes.Buffer
	if err := m.compose(ctx, r, nil, &out, "ps", "--all", "--quiet", "db"); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if !containerIDPattern.MatchString(id) {
		return "", fmt.Errorf("the container of %s is missing", r.Name)
	}
	return id, nil
}

// Remove a PostgreSQL replica's slot from its source, ending the replica's
// connection first if it is still open.
func (m *manager) dropSlot(ctx context.Context, src record, slot string) error {
	return m.admin(ctx, src, fmt.Sprintf(`DO $$ BEGIN
  FOR i IN 1..20 LOOP
    IF NOT EXISTS (SELECT FROM pg_replication_slots WHERE slot_name = '%[1]s') THEN RETURN; END IF;
    IF NOT (SELECT active FROM pg_replication_slots WHERE slot_name = '%[1]s') THEN
      PERFORM pg_drop_replication_slot('%[1]s');
      RETURN;
    END IF;
    PERFORM pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = '%[1]s';
    PERFORM pg_sleep(0.5);
  END LOOP;
END $$;
`, slot))
}

// Drop the slots of replicas that are gone, for example deleted while the
// source was stopped, so they no longer keep WAL.
func (m *manager) dropStaleSlots(ctx context.Context, src record) {
	reps, err := m.replicasOf(src.Name)
	if err != nil {
		return
	}
	keep := []string{"''"}
	for _, r := range reps {
		keep = append(keep, "'"+slotName(r)+"'")
	}
	sql := fmt.Sprintf("SELECT count(pg_drop_replication_slot(slot_name)) FROM pg_replication_slots WHERE slot_name LIKE 'openrds\\_%%' AND NOT active AND slot_name NOT IN (%s);\n", strings.Join(keep, ", "))
	if err := m.admin(ctx, src, sql); err != nil {
		log.Printf("removing unused replication slots of %s: %v", src.Name, err)
	}
}

// After a Valkey source restarts, its replicas follow its TLS setting.
func (m *manager) refollow(ctx context.Context, src record) {
	reps, err := m.replicasOf(src.Name)
	if err != nil || len(reps) == 0 {
		return
	}
	want := "no"
	if src.RequireTLS {
		want = "yes"
	}
	for _, r := range reps {
		id, err := m.runningContainer(ctx, r)
		if err != nil {
			continue
		}
		out, err := m.engineClient(ctx, r, id, valkeyClient, r.Password+"\nCONFIG GET tls-replication\n")
		if lines := nonEmptyLines(out); err != nil || len(lines) == 0 || lines[len(lines)-1] == want {
			continue
		}
		if err := m.writeValkeyUsers(ctx, r); err == nil {
			err = m.admin(ctx, r, "CONFIG SET tls-replication "+want+"\nCLIENT KILL TYPE master\n")
		}
		if err != nil {
			log.Printf("pointing replica %s at %s again: %v", r.Name, src.Name, err)
		}
	}
}

// Apply a change of users or passwords on the source to its running
// replicas: Valkey does not replicate users, and poolers keep a list of them.
// PostgreSQL and MySQL replicate the change itself; sessions of a removed
// user end on the replicas too.
func (m *manager) propagateAccess(ctx context.Context, before, after record, script string) error {
	reps, err := m.replicasOf(after.Name)
	if err != nil || len(reps) == 0 {
		return err
	}
	states, err := m.states(ctx)
	if err != nil {
		return fail(503, "Cannot read database status: "+err.Error())
	}
	dropped := ""
	for _, u := range before.Users {
		if after.findUser(u.Name) < 0 {
			dropped = u.Name
		}
	}
	var failed []string
	for _, rep := range reps {
		if states[rep.Project] != "running" {
			continue // a stopped replica gets the change when it starts
		}
		was, now := rep, rep
		was.follow(before)
		now.follow(after)
		var err error
		switch {
		case rep.Engine == "valkey":
			if err = m.admin(ctx, was, script); err == nil {
				err = m.writeValkeyUsers(ctx, now)
			}
		case dropped != "" && rep.Engine == "postgres":
			// Wait until the replica has replayed the drop, then end its sessions.
			err = m.admin(ctx, now, fmt.Sprintf("SELECT pg_sleep(1);\nSELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = '%s';\n", dropped))
		case dropped != "" && rep.Engine == "mysql":
			err = m.endMySQLSessions(ctx, now, dropped)
		}
		if err == nil && now.Pooler.on() {
			err = m.reloadPoolerUsers(ctx, now)
		}
		if err != nil {
			log.Printf("applying a user change to replica %s: %v", rep.Name, err)
			failed = append(failed, rep.Name)
		}
	}
	if len(failed) > 0 {
		return fail(502, fmt.Sprintf("The change is active on %s. Read replicas %s get it when they restart.", after.Name, strings.Join(failed, ", ")))
	}
	return nil
}

// Make a read replica a standalone database that accepts writes. It keeps
// its data, users and settings; its source is not changed.
func (m *manager) promote(ctx context.Context, name string) (err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	if r.ReplicaOf == "" {
		return fail(409, fmt.Sprintf("%s is not a read replica.", r.Name))
	}
	if r.PendingRestore {
		return fail(409, "This replica was never completed. Delete it instead.")
	}
	states, err := m.states(ctx)
	if err != nil {
		return fail(503, "Cannot read database status: "+err.Error())
	}
	if states[r.Project] != "running" {
		return fail(409, fmt.Sprintf("Start %s and wait until it is healthy: promotion runs in the database.", r.Name))
	}
	source := r.ReplicaOf
	finish := m.trackDetail(ctx, name, "Promote read replica", "was a replica of "+source)
	defer func() { finish(err) }()
	next := r
	next.ReplicaOf, next.ReplicationPassword, next.sourceTLS, next.SourceAddress, next.SourceRoute = "", "", false, "", ""
	switch r.Engine {
	case "postgres":
		err = m.admin(ctx, r, pgPromoteSQL)
	case "mysql":
		err = m.compose(ctx, r, nil, io.Discard, "exec", "-T", "db", "sh", "-ec", mysqlPromoteScript)
	default:
		// The configuration file loses replicaof first, so a restart cannot
		// turn the promoted database back into a replica.
		if err = m.writeValkeyUsers(ctx, next); err == nil {
			if err = m.admin(ctx, r, "REPLICAOF NO ONE\n"); err != nil {
				m.writeValkeyUsers(ctx, r)
			}
		}
	}
	if err != nil {
		return fail(502, fmt.Sprintf("Promotion did not finish; %s is still a read replica of %s. %s", r.Name, source, err))
	}
	if err := m.save(next); err != nil {
		return err
	}
	if src, err := m.record(source); err == nil {
		if r.SourceRoute == "" {
			m.unlinkReplica(ctx, next, source)
		} else if err := m.applySourceAccess(ctx, src); err != nil {
			log.Printf("closing %s to the server of %s: %v", source, r.Name, err)
		}
		if r.Engine == "postgres" {
			if err := m.dropSlot(ctx, src, slotName(r)); err != nil {
				log.Printf("removing the replication slot of %s from %s: %v", r.Name, source, err)
			}
		}
	}
	m.forgetReplication(r)
	return nil
}

// PostgreSQL leaves recovery at once, then forgets its source.
const pgPromoteSQL = `DO $$ BEGIN
  IF pg_is_in_recovery() AND NOT pg_promote(true, 300) THEN
    RAISE EXCEPTION 'promotion did not finish within 5 minutes';
  END IF;
END $$;
ALTER SYSTEM RESET primary_conninfo;
ALTER SYSTEM RESET primary_slot_name;
SELECT pg_reload_conf();
ALTER ROLE openrds_replication PASSWORD NULL;
`

// MySQL first applies what it already received, for up to 5 minutes.
const mysqlPromoteScript = `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
mysql -u root -e "STOP REPLICA IO_THREAD"
i=0
while [ $i -lt 300 ]; do
  s=$(mysql -u root -e "SHOW REPLICA STATUS\G")
  case "$s" in *"Replica_SQL_Running: No"*|*"read all relay log"*|"") break ;; esac
  i=$((i + 1))
  sleep 1
done
mysql -u root -e "STOP REPLICA; RESET REPLICA ALL; SET PERSIST super_read_only = OFF; SET PERSIST read_only = OFF; SET GLOBAL event_scheduler = ON"`

// ---------- status

const pgReplicaSQL = `SELECT /* openrds */ coalesce((SELECT status FROM pg_stat_wal_receiver LIMIT 1), 'none'),
  CASE WHEN pg_last_wal_receive_lsn() IS NULL THEN NULL
       WHEN pg_last_wal_receive_lsn() <= pg_last_wal_replay_lsn() THEN 0
       ELSE greatest(extract(epoch FROM now() - pg_last_xact_replay_timestamp()), 0) END;
`

const mysqlStatusClient = `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -u root -B`

// Read what a running replica is doing.
func (m *manager) replicationStatus(ctx context.Context, r record, container string) replicationStatus {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st := replicationStatus{State: "connecting", At: time.Now().UTC()}
	number := func(s string) *float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil
		}
		v = max(v, 0)
		return &v
	}
	switch r.Engine {
	case "postgres":
		out, err := m.engineClient(ctx, r, container, pgClient, pgReplicaSQL)
		f := strings.Split(strings.TrimSpace(out), "|")
		if err != nil || len(f) != 2 {
			return replicationStatus{State: "unknown", Detail: "The replica did not answer.", At: st.At}
		}
		if f[0] == "streaming" {
			st.State, st.Lag = "replicating", number(f[1])
			return st
		}
		st.Detail = m.lastProblem(ctx, r)
		if src, err := m.record(r.ReplicaOf); err == nil {
			if id, err := m.runningContainer(ctx, src); err == nil {
				out, err := m.engineClient(ctx, src, id, pgClient, fmt.Sprintf("SELECT /* openrds */ coalesce((SELECT wal_status FROM pg_replication_slots WHERE slot_name = '%s'), 'missing');\n", slotName(r)))
				if s := strings.TrimSpace(out); err == nil && (s == "lost" || s == "missing") {
					st.State, st.Detail = "broken", fmt.Sprintf("%s no longer keeps the changes this replica needs: it fell too far behind. Delete it and create a new replica.", r.ReplicaOf)
				}
			} else {
				st.Detail = fmt.Sprintf("Waiting for %s to run.", r.ReplicaOf)
			}
		}
	case "mysql":
		out, err := m.engineClient(ctx, r, container, mysqlStatusClient, "SHOW REPLICA STATUS;\n")
		v, ok := replicaFields(out)
		if err != nil || !ok {
			return replicationStatus{State: "unknown", Detail: "The replica did not answer.", At: st.At}
		}
		problem := strings.TrimSpace(v["Last_SQL_Error"])
		if problem == "" {
			problem = strings.TrimSpace(v["Last_IO_Error"])
		}
		switch io, sql := v["Replica_IO_Running"], v["Replica_SQL_Running"]; {
		case io == "Yes" && sql == "Yes":
			st.State, st.Lag = "replicating", number(v["Seconds_Behind_Source"])
		case io == "Connecting" && sql == "Yes":
			st.Detail = problem
		default:
			st.State, st.Detail = "broken", problem
		}
		st.Detail = strings.ReplaceAll(st.Detail, `\n`, " ")
	case "valkey":
		out, err := m.engineClient(ctx, r, container, valkeyClient, r.Password+"\nINFO replication\n")
		if err != nil || !strings.Contains(out, "role:") {
			return replicationStatus{State: "unknown", Detail: "The replica did not answer.", At: st.At}
		}
		v := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if k, val, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
				v[k] = val
			}
		}
		switch {
		case v["master_link_status"] == "up":
			st.State = "replicating"
			zero := 0.0
			st.Lag = &zero // Valkey applies changes as they arrive
		case v["master_sync_in_progress"] == "1":
			st.State, st.Detail = "syncing", fmt.Sprintf("Copying the data of %s.", r.ReplicaOf)
		default:
			st.Detail = m.lastProblem(ctx, r)
		}
	}
	return st
}

// The newest line in the database log that reports a problem.
func (m *manager) lastProblem(ctx context.Context, r record) string {
	out := tailBuffer{limit: 32 << 10}
	if m.compose(ctx, r, nil, &out, "logs", "--no-color", "--no-log-prefix", "--tail", "40", "db") != nil {
		return ""
	}
	lines := strings.Split(string(out.data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.Contains(line, "FATAL") || strings.Contains(line, "ERROR") || strings.Contains(line, "Error condition") || strings.Contains(line, "Unable to connect") {
			for _, s := range []string{r.Password, r.ReplicationPassword} {
				if s != "" {
					line = strings.ReplaceAll(line, s, "[redacted]")
				}
			}
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			return line
		}
	}
	return ""
}

// Poll a new replica until it follows its source, or followWait passes.
func (m *manager) awaitReplication(ctx context.Context, r record) replicationStatus {
	deadline := time.Now().Add(followWait)
	var st replicationStatus
	for {
		if id, err := m.runningContainer(ctx, r); err == nil {
			st = m.replicationStatus(ctx, r, id)
		} else {
			st = replicationStatus{State: "stopped", At: time.Now().UTC()}
		}
		m.noteReplication(r.Name, st)
		// A large Valkey dataset may still be copying at the deadline, which is fine.
		if st.State == "replicating" || st.State == "broken" || time.Now().After(deadline) || ctx.Err() != nil {
			return st
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *manager) noteReplication(name string, st replicationStatus) {
	m.replication.mu.Lock()
	defer m.replication.mu.Unlock()
	if m.replication.status == nil {
		m.replication.status = make(map[string]replicationStatus)
	}
	m.replication.status[name] = st
}

func (m *manager) cachedReplication(name string) (replicationStatus, bool) {
	m.replication.mu.Lock()
	defer m.replication.mu.Unlock()
	st, ok := m.replication.status[name]
	return st, ok
}

func (m *manager) forgetReplication(r record) {
	m.replication.mu.Lock()
	defer m.replication.mu.Unlock()
	delete(m.replication.status, r.Name)
	delete(m.replication.marks, r.Project)
}

// Check every running replica and alert when one stops following its source,
// cannot reach it for a while, or falls far behind; and when it recovers.
// The first check after the console starts only takes note.
func (m *manager) watchReplication(ctx context.Context) {
	if !m.replication.watching.TryLock() {
		return
	}
	defer m.replication.watching.Unlock()
	all, err := m.records()
	if err != nil {
		return
	}
	var reps []record
	for _, r := range all {
		if r.ReplicaOf != "" && !r.PendingRestore {
			reps = append(reps, r)
		}
	}
	if len(reps) == 0 {
		return
	}
	states, err := m.states(ctx)
	if err != nil {
		return
	}
	projects := map[string]string{}
	for _, r := range all {
		projects[r.Name] = r.Project
	}
	m.mu.Lock()
	busy := maps.Clone(m.busy)
	m.mu.Unlock()
	now := time.Now().UTC()
	type finding struct {
		e event
		a alert
	}
	var findings []finding
	for _, r := range reps {
		if busy[r.Name] || busy[r.ReplicaOf] || states[r.Project] != "running" {
			continue
		}
		st, fresh := m.cachedReplication(r.Name)
		if !fresh || now.Sub(st.At) > 45*time.Second {
			id, err := m.runningContainer(ctx, r)
			if err != nil {
				continue
			}
			st = m.replicationStatus(ctx, r, id)
			m.noteReplication(r.Name, st)
		}
		if st.State == "unknown" {
			continue
		}
		raise := func(severity, title, action, result, detail string) {
			findings = append(findings, finding{
				event{Name: r.Name, Action: action, Result: result, Detail: detail},
				alert{Category: categoryHealth, Severity: severity, Database: r.Name, Title: title, Detail: detail},
			})
		}
		m.replication.mu.Lock()
		if m.replication.marks == nil {
			m.replication.marks = make(map[string]replicaMark)
		}
		mark, seen := m.replication.marks[r.Project]
		next := replicaMark{state: st.State, lagging: mark.lagging, unreachable: mark.unreachable, alerted: mark.alerted}
		lag := time.Duration(0)
		if st.Lag != nil {
			lag = time.Duration(*st.Lag * float64(time.Second))
		}
		if st.State != "connecting" {
			next.unreachable, next.alerted = time.Time{}, false
		} else if next.unreachable.IsZero() {
			next.unreachable = now
		}
		sourceUp := states[projects[r.ReplicaOf]] == "running"
		switch {
		case !seen:
			next.lagging = lag >= lagAlert
		case st.State == "broken" && mark.state != "broken":
			raise("critical", r.Name+" stopped replicating from "+r.ReplicaOf, "Replication stopped", "failed", st.Detail)
		case st.State == "connecting" && sourceUp && !mark.alerted && now.Sub(next.unreachable) >= unreachableAlert:
			next.alerted = true
			raise("warning", r.Name+" cannot reach "+r.ReplicaOf, "Replication interrupted", "failed",
				fmt.Sprintf("It has not received changes for %d minutes. %s", int(now.Sub(next.unreachable).Minutes()), st.Detail))
		case st.State == "replicating" && lag >= lagAlert && !mark.lagging:
			next.lagging = true
			raise("warning", fmt.Sprintf("%s is %d minutes behind %s", r.Name, int(lag.Minutes()), r.ReplicaOf), "Replica lagging", "failed",
				"Reads from it return older data. Long queries on the replica, a smaller instance than the source, or heavy writes on the source cause this.")
		case st.State == "replicating" && (mark.state == "broken" || mark.alerted || mark.lagging && lag < lagCleared):
			next.lagging = false
			raise("resolved", r.Name+" is replicating again", "Replicating again", "completed", "")
		}
		m.replication.marks[r.Project] = next
		m.replication.mu.Unlock()
	}
	for _, f := range findings {
		m.raise(ctx, f.e, f.a)
	}
}

// ---------- API

func replicaRoutes(add func(string, int, endpoint), m *manager) {
	add("POST /api/instances/{name}/replicas", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var req replicaRequest
		if err := decode(w, r, &req); err != nil {
			return err
		}
		if err := m.createReplica(r.Context(), r.PathValue("name"), req); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": req.Name})
		return nil
	})
	add("POST /api/instances/{name}/promote", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.promote(r.Context(), r.PathValue("name")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("GET /api/instances/{name}/replication", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		db, err := m.record(r.PathValue("name"))
		if err != nil {
			return err
		}
		if db.ReplicaOf == "" {
			return fail(404, db.Name+" is not a read replica.")
		}
		st := replicationStatus{State: "stopped", At: time.Now().UTC()}
		if db.PendingRestore {
			st.State, st.Detail = "broken", "The replica was never completed. Delete it and try again."
		} else if id, err := m.runningContainer(r.Context(), db); err == nil {
			st = m.replicationStatus(r.Context(), db, id)
			m.noteReplication(db.Name, st)
		}
		sendJSON(w, 200, st)
		return nil
	})
}
