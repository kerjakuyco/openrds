package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type engine struct {
	title          string
	scheme         string
	port           int
	hostPort       int // suggested first host port
	minMemory      int // MiB
	dataDir        string
	user           string
	database       string
	suffix         string
	restoreStopped bool
	dumpAuth       bool // the dump reads the owner password from its first input line
	compress       bool // the dump is plain text, so backups store it gzip-compressed
	dump           []string
	restore        []string
	parameters     []parameter
}

var engineOrder = []string{"postgres", "mysql", "valkey"}

// Only these templates and commands can be run by the control plane.
// Compose exec -T keeps database dumps binary-safe in both directions.
var engines = map[string]engine{
	"postgres": {
		title: "PostgreSQL", scheme: "postgresql", port: 5432, hostPort: 15432, minMemory: 128, dataDir: "/var/lib/postgresql/data", user: "openrds", database: "app", suffix: ".dump",
		dump:       []string{"pg_dump", "-U", "openrds", "-d", "app", "--format=custom"},
		restore:    []string{"pg_restore", "-U", "openrds", "-d", "app", "--single-transaction", "--no-owner", "--no-privileges"},
		parameters: postgresParameters,
	},
	"mysql": {
		title: "MySQL", scheme: "mysql", port: 3306, hostPort: 13306, minMemory: 512, dataDir: "/var/lib/mysql", user: "openrds", database: "app", suffix: ".sql",
		parameters: mysqlParameters,
		compress:   true,
		// Keep the password out of argv. Root is used locally inside the
		// container so routine, event and trigger definitions are included.
		dump:    []string{"sh", "-ec", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysqldump -u root --single-transaction --quick --no-tablespaces --set-gtid-purged=OFF --routines --events --triggers --hex-blob app`},
		restore: []string{"sh", "-ec", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql --binary-mode -u root app`},
	},
	"valkey": {
		title: "Valkey", scheme: "redis", port: 6379, hostPort: 16379, minMemory: 128, dataDir: "/data", user: "default", database: "0", suffix: ".rdb",
		restoreStopped: true,
		dumpAuth:       true,
		parameters:     valkeyParameters,
		// Progress from valkey-cli stays out of the backup stream.
		dump: []string{"sh", "-ec", `IFS= read -r REDISCLI_AUTH; export REDISCLI_AUTH; valkey-cli --no-auth-warning $OPENRDS_CLI_FLAGS --rdb /tmp/openrds.rdb >/dev/null && exec cat /tmp/openrds.rdb`},
		// A running Valkey loads AOF ahead of RDB. Install the snapshot only
		// while the server is stopped, then start it so the snapshot is loaded.
		restore: []string{"sh", "-ec", `set -eu
cat > /data/dump.rdb.new
magic=$(head -c 5 /data/dump.rdb.new)
[ "$magic" = REDIS ] || [ "$magic" = VALKE ] # newer Valkey files start with VALKEY
mv /data/dump.rdb.new /data/dump.rdb
rm -rf /data/appendonlydir /data/appendonly.aof`},
	},
}

type dockerRunner interface {
	run(ctx context.Context, env []string, in io.Reader, out io.Writer, args ...string) error
}

type dockerCLI struct{}

// Keep the useful end of Compose diagnostics without buffering image-pull logs.
type tailBuffer struct {
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n > b.limit {
		p = p[n-b.limit:]
	}
	if drop := len(b.data) + len(p) - b.limit; drop > 0 {
		b.data = b.data[drop:]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (dockerCLI) run(ctx context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	diagnostics := tailBuffer{limit: 2048}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &diagnostics
	// Database engines commonly write their normal server logs to stderr.
	// Using the same comparable writer lets os/exec serialize both streams.
	if len(args) > 0 && args[0] == "compose" && slices.Contains(args, "logs") {
		cmd.Stderr = out
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		text := strings.TrimSpace(string(diagnostics.data))
		for _, v := range env {
			key, value, _ := strings.Cut(v, "=")
			if (strings.HasSuffix(key, "PASSWORD") || strings.HasSuffix(key, "_KEY")) && value != "" {
				text = strings.ReplaceAll(text, value, "[redacted]")
			}
		}
		if text == "" {
			text = err.Error()
		}
		return fmt.Errorf("docker: %s", text)
	}
	return nil
}

// The /dev/shm size: as large as the memory limit, since its pages count
// against that limit anyway, or 1 GiB when memory is unlimited.
func shmBytes(memoryMiB int) string {
	if memoryMiB <= 0 {
		return strconv.FormatInt(1<<30, 10)
	}
	return strconv.FormatInt(int64(memoryMiB)<<20, 10)
}

// The database's name on its Docker network; another one while an upgrade
// fills it, so clients do not find it early.
func dbAlias(r record) string {
	if r.staging {
		return "openrds-" + r.Name + "-upgrading"
	}
	return "openrds-" + r.Name
}

func valkeyMaxMemory(memoryMiB int) string {
	if memoryMiB <= 0 {
		return "0"
	}
	return strconv.FormatInt(int64(memoryMiB)*1024*1024*3/4, 10)
}

func (m *manager) compose(ctx context.Context, r record, in io.Reader, out io.Writer, args ...string) error {
	if _, ok := engines[r.Engine]; !ok {
		return fmt.Errorf("unknown stored engine %q", r.Engine)
	}
	base := []string{"compose", "--project-name", r.Project, "--file", filepath.Join(m.templates, r.Engine+".yaml")}
	env := []string{
		"OPENRDS_DB_PORT=" + strconv.Itoa(r.Port),
		"OPENRDS_DB_IMAGE=" + r.image(),
		"OPENRDS_DATA_MOUNT=" + r.dataMount(),
		"OPENRDS_DB_ALIAS=" + dbAlias(r),
		"OPENRDS_DB_CPUS=" + strconv.FormatFloat(r.CPUs, 'f', -1, 64),
		"OPENRDS_DB_MEMORY=" + strconv.FormatInt(int64(r.MemoryMiB)*1024*1024, 10),
		"OPENRDS_DB_SHM=" + shmBytes(r.MemoryMiB),
		"OPENRDS_DB_PASSWORD=" + r.Password,
		"OPENRDS_ROOT_PASSWORD=" + r.RootPassword,
		"OPENRDS_VALKEY_MAXMEMORY=" + valkeyMaxMemory(r.MemoryMiB),
	}
	if r.DiskGiB > 0 {
		base = append(base, "--file", filepath.Join(m.templates, "volume-quota.yaml"))
		env = append(env, "OPENRDS_DISK_LINK="+r.diskKey())
	}
	if r.PITR && r.Engine == "postgres" {
		base = append(base, "--file", filepath.Join(m.templates, "pitr.yaml"))
	}
	if r.staging {
		base = append(base, "--file", filepath.Join(m.templates, "upgrade.yaml"))
	}
	srv, ok := m.server(r.Server)
	if !ok {
		return fmt.Errorf("server %s is not registered", r.Server)
	}
	r.serverHosts = slices.DeleteFunc([]string{srv.PrivateAddress, srv.PublicAddress}, func(h string) bool { return h == "" })
	files, err := m.injected(r)
	if err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(files)) {
		env = append(env, k+"="+files[k])
	}
	if r.Engine == "valkey" && r.RequireTLS {
		env = append(env, "OPENRDS_VALKEY_CLI_FLAGS=--tls --insecure")
	}
	if r.Pooler.on() {
		image, err := m.poolerImage(ctx, r.Server)
		if err != nil {
			return err
		}
		base = append(base, "--file", filepath.Join(m.templates, "pooler.yaml"))
		env = append(env, "OPENRDS_POOLER_IMAGE="+image, "OPENRDS_POOLER_PORT="+strconv.Itoa(r.Pooler.Port), "OPENRDS_POOLER_ALIAS="+poolerAlias(r))
	}
	// Each container is recreated only when its own files change: a new
	// database user reloads the pooler, not the database.
	dbFiles, poolerFiles := maps.Clone(files), map[string]string{}
	for _, k := range []string{"OPENRDS_POOLER_INI", "OPENRDS_POOLER_USERS"} {
		if v, ok := dbFiles[k]; ok {
			poolerFiles[k] = v
			delete(dbFiles, k)
		}
	}
	poolerFiles["OPENRDS_TLS_CERT"], poolerFiles["OPENRDS_TLS_KEY"] = files["OPENRDS_TLS_CERT"], files["OPENRDS_TLS_KEY"]
	path, err := m.writeOverride(r, digest(dbFiles), digest(poolerFiles))
	if err != nil {
		return fmt.Errorf("writing the Compose override: %w", err)
	}
	base = append(base, "--file", path)
	// Replicas on other servers are let in beside the rules people set.
	replicasPrivate, replicasPublic := m.replicaSources(r)
	privateOn := (r.PrivateAccess || len(replicasPrivate) > 0) && srv.PrivateAddress != ""
	if r.PublicAccess || privateOn {
		image, err := m.gatewayImage(ctx)
		if err != nil {
			return err
		}
		env = append(env, "OPENRDS_GATEWAY_IMAGE="+image, "OPENRDS_GATEWAY_TARGET=db:"+strconv.Itoa(engines[r.Engine].port))
	}
	if r.PublicAccess {
		base = append(base, "--file", filepath.Join(m.templates, "gateway.yaml"))
		env = append(env, "OPENRDS_GATEWAY_PUBLISH="+strconv.Itoa(r.PublicPort), "OPENRDS_GATEWAY_ALLOW="+joinAllow(allowList(r.Firewall), replicasPublic))
	}
	if privateOn {
		rules := ""
		if r.PrivateAccess {
			rules = allowList(r.PrivateFirewall)
		}
		base = append(base, "--file", filepath.Join(m.templates, "private-gateway.yaml"))
		env = append(env, "OPENRDS_PRIVATE_BIND="+srv.PrivateAddress, "OPENRDS_PRIVATE_ALLOW="+joinAllow(rules, replicasPrivate))
	}
	return m.on(r).run(ctx, env, in, out, append(base, args...)...)
}

// The state of every database by Compose project. Databases on a server the
// console can't reach are "unreachable".
// An allowlist with more sources added.
func joinAllow(rules string, more []string) string {
	return strings.Join(slices.DeleteFunc(append([]string{rules}, more...), func(s string) bool { return s == "" }), ",")
}

func (m *manager) states(ctx context.Context) (map[string]string, error) {
	states, err := m.statesFor(ctx, "io.openrds.database=true")
	if err != nil {
		return nil, err
	}
	if down := m.unreachable(); len(down) > 0 {
		records, err := m.records()
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			if down[r.Server] && states[r.Project] == "" {
				states[r.Project] = "unreachable"
			}
		}
	}
	return states, nil
}

// The state of the containers with a label on every server, by Compose
// project. The console's own server must answer; others that don't are
// noted as unreachable and left out.
func (m *manager) statesFor(ctx context.Context, label string) (map[string]string, error) {
	list := m.serverList()
	outputs := make([]string, len(list))
	errs := make([]error, len(list))
	var wg sync.WaitGroup
	for i, s := range list {
		if s.ID != localServer && m.recentlyDown(s.ID) {
			errs[i] = errors.New("unreachable")
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			call := ctx
			if s.ID != localServer {
				var cancel context.CancelFunc
				call, cancel = context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
			}
			var out bytes.Buffer
			if lister, ok := m.localContainers(s.ID); ok {
				var list []listedContainer
				list, errs[i] = lister.containers(call, label, false)
				for _, c := range list {
					fmt.Fprintf(&out, "%s|%s|%s\n", c.Project, c.State, c.Status)
				}
			} else {
				errs[i] = m.dockerFor(s.ID).run(call, nil, nil, &out, "ps", "--all", "--filter", "label="+label,
					"--format", `{{.Label "com.docker.compose.project"}}|{{.State}}|{{.Status}}`)
			}
			outputs[i] = out.String()
			if s.ID != localServer && ctx.Err() == nil {
				m.noteReachable(s.ID, errs[i])
			}
		}()
	}
	wg.Wait()
	if errs[0] != nil {
		return nil, errs[0]
	}
	var lines []string
	for i := range list {
		if errs[i] == nil {
			lines = append(lines, strings.Split(outputs[i], "\n")...)
		}
	}
	states := make(map[string]string)
	for _, line := range lines {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(fields) != 3 {
			continue
		}
		state := fields[1]
		switch {
		case state == "exited":
			state = "stopped"
		case state == "running" && strings.Contains(fields[2], "(unhealthy)"):
			state = "unhealthy"
		case state == "running" && strings.Contains(fields[2], "(health: starting)"):
			state = "starting"
		}
		states[fields[0]] = state
	}
	return states, nil
}
