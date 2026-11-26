package main

// Connection pooling for PostgreSQL, like RDS Proxy: PgBouncer runs in its
// own container next to the database, so many client connections share a
// few server connections. Apps with many processes, workers or serverless
// functions stay under max_connections, and opening a connection is cheap.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type poolerSpec struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode,omitempty"`       // transaction or session
	PoolSize   int    `json:"poolSize,omitempty"`   // server connections per user and database
	MaxClients int    `json:"maxClients,omitempty"` // client connections it accepts
	Port       int    `json:"port,omitempty"`       // on the host's loopback interface
}

const (
	defaultPoolMode   = "transaction"
	defaultPoolSize   = 20
	defaultMaxClients = 1000
	maxPoolerClients  = 50000 // within the container's open-file limit
	// Server connections the pooler leaves for direct connections, backups
	// and OpenRDS itself.
	poolerReserve = 10
)

func (p *poolerSpec) on() bool { return p != nil && p.Enabled }

func poolerAlias(r record) string { return "openrds-" + r.Name + "-pooler" }

// Defaults for a pooler being turned on, and none for one turned off.
func normalizePooler(s *instanceSpec, all []record) {
	p := s.Pooler
	if p == nil {
		return
	}
	if !p.Enabled {
		s.Pooler = nil
		return
	}
	if p.Mode == "" {
		p.Mode = defaultPoolMode
	}
	if p.PoolSize == 0 {
		p.PoolSize = defaultPoolSize
	}
	if p.MaxClients == 0 {
		p.MaxClients = defaultMaxClients
	}
	if p.Port == 0 {
		p.Port = suggestPoolerPort(all, *s)
	}
}

func checkPooler(s instanceSpec) error {
	p := s.Pooler
	if !p.on() {
		return nil
	}
	if s.Engine != "postgres" {
		return fail(400, "Connection pooling is available for PostgreSQL.")
	}
	if p.Mode != "transaction" && p.Mode != "session" {
		return fail(400, "Choose transaction or session pooling.")
	}
	if p.PoolSize < 1 || p.PoolSize > 1000 {
		return fail(400, "Use a pool of 1–1000 server connections.")
	}
	if p.MaxClients < 10 || p.MaxClients > maxPoolerClients {
		return fail(400, fmt.Sprintf("Accept 10–%d client connections.", maxPoolerClients))
	}
	if p.Port < 1024 || p.Port > 65535 {
		return fail(400, "Choose a pooler port between 1024 and 65535.")
	}
	if p.Port == s.Port || s.PublicAccess && p.Port == s.PublicPort {
		return fail(400, "Use a pooler port that differs from the database’s ports.")
	}
	return nil
}

// PgBouncer's usual port when it is free, otherwise the next free one.
func suggestPoolerPort(all []record, s instanceSpec) int {
	for port := 6432; port < 65535; port++ {
		probe := s
		probe.Pooler = &poolerSpec{Enabled: true, Port: port}
		if port != s.Port && !(s.PublicAccess && port == s.PublicPort) && checkPorts(all, probe) == nil {
			return port
		}
	}
	return 0
}

// Server connections the pooler may open: max_connections less a reserve.
func maxDBConnections(r record) int {
	n, err := strconv.Atoi(r.effectiveParameters()["max_connections"])
	if err != nil || n <= 0 {
		n = 100 // PostgreSQL's default
	}
	return max(n-poolerReserve, (n+1)/2)
}

// The PgBouncer configuration. Clients use TLS when they ask for it, or
// always when the database requires it; so does the pooler towards the
// database. Prepared statements work in transaction mode.
func poolerIni(r record) string {
	p := r.Pooler
	tlsMode := "prefer"
	if r.RequireTLS {
		tlsMode = "require"
	}
	lines := []string{
		"; Managed by OpenRDS. Changes here are replaced when the pooler restarts.",
		"[databases]",
		"* = host=db port=5432",
		"",
		"[pgbouncer]",
		"listen_addr = 0.0.0.0",
		"listen_port = 6432",
		"unix_socket_dir =",
		"auth_type = scram-sha-256",
		"auth_file = /run/openrds/userlist.txt",
		"pool_mode = " + p.Mode,
		"default_pool_size = " + strconv.Itoa(p.PoolSize),
		"max_client_conn = " + strconv.Itoa(p.MaxClients),
		"max_db_connections = " + strconv.Itoa(maxDBConnections(r)),
		"max_prepared_statements = 200",
		"ignore_startup_parameters = extra_float_digits",
		"server_tls_sslmode = " + tlsMode,
		"client_tls_sslmode = " + tlsMode,
		"client_tls_cert_file = /run/openrds/server.crt",
		"client_tls_key_file = /run/openrds/server.key",
		"admin_users =",
		"log_connections = 0",
		"log_disconnections = 0",
	}
	return strings.Join(lines, "\n") + "\n"
}

// The users PgBouncer accepts, with the passwords it also uses towards the
// database. Passwords use a quote-free character set.
func poolerUsers(r record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q %q\n", engines[r.Engine].user, r.Password)
	for _, u := range r.Users {
		fmt.Fprintf(&b, "%q %q\n", u.Name, u.Password)
	}
	return b.String()
}

func (m *manager) poolerImage(ctx context.Context) (string, error) {
	image := env("OPENRDS_POOLER_IMAGE", "openrds-pooler:local")
	if m.docker.run(ctx, nil, nil, io.Discard, "image", "inspect", "--format", "{{.Id}}", image) != nil {
		return "", fail(409, "The connection pooler image "+image+" is missing. Run sh install.sh again, or docker compose --profile helper build pooler, then retry.")
	}
	return image, nil
}

// Start, update or remove the pooler to match the record. Compose recreates
// it when its settings change; the database keeps running.
func (m *manager) applyPooler(ctx context.Context, previous, next record, databaseLive bool) error {
	if next.Pooler.on() && databaseLive {
		return m.compose(ctx, next, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "60", "pooler")
	}
	if previous.Pooler.on() {
		return m.compose(ctx, previous, nil, nil, "rm", "--stop", "--force", "pooler")
	}
	return nil
}

// Write the users file into the running pooler and reload it, so a new
// password works through the pooler at once and pooled clients stay
// connected. A recreated pooler gets the same file from Compose.
//
// PgBouncer rereads the file only when its modification time or size
// changed, and two password changes in one second leave both the same, so
// the time moves forward by at least a second each write.
func (m *manager) reloadPoolerUsers(ctx context.Context, r record) error {
	return m.compose(ctx, r, strings.NewReader(poolerUsers(r)), nil, "exec", "-T", "pooler", "sh", "-ec", `f=/run/openrds/userlist.txt
before=$(stat -c %Y "$f")
cat > "$f"
now=$(date +%s)
[ "$now" -gt "$before" ] || now=$((before + 1))
touch -d "@$now" "$f"
kill -HUP 1`)
}

func samePooler(a, b *poolerSpec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
