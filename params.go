package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// An engine setting the API accepts. Values are checked against their type
// before they reach a database command line, so a typo cannot inject options.
type parameter struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // integer, real, size, enum, list, flags or schedule
	Default     string   `json:"default"`
	Min         float64  `json:"min"` // bytes for a size
	Max         float64  `json:"max"`
	Unit        string   `json:"unit,omitempty"`
	Values      []string `json:"values,omitempty"`
	Description string   `json:"description"`
	Auto        string   `json:"auto,omitempty"` // how OpenRDS sizes it to the memory limit
	memoryBound bool     // a cache that must fit inside the memory limit
}

var (
	onOff    = []string{"on", "off"}
	onOffSQL = []string{"ON", "OFF"}
	yesNo    = []string{"yes", "no"}

	integerPattern  = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,11})$`)
	realPattern     = regexp.MustCompile(`^(0|[1-9][0-9]{0,7})(\.[0-9]{1,6})?$`)
	digitsPattern   = regexp.MustCompile(`^[1-9][0-9]{0,12}$`)
	flagsPattern    = regexp.MustCompile(`^[KEg$lshzxetmdnA]{0,16}$`)
	schedulePattern = regexp.MustCompile(`^$|^[1-9][0-9]{0,8} [1-9][0-9]{0,8}( [1-9][0-9]{0,8} [1-9][0-9]{0,8}){0,7}$`)
)

// Units in increasing powers of 1024, as each engine spells them.
var sizeUnits = map[string][]string{"postgres": {"kB", "MB", "GB", "TB"}, "mysql": {"K", "M", "G"}}

var postgresParameters = []parameter{
	{Name: "max_connections", Type: "integer", Default: "100", Min: 5, Max: 10000, Unit: "connections",
		Description: "Maximum concurrent client connections. Each connection reserves some memory."},
	{Name: "shared_buffers", Type: "size", Default: "128MB", Min: 16 << 20, Max: 1 << 40, memoryBound: true, Auto: "25% of the memory limit",
		Description: "Memory for caching table and index pages."},
	{Name: "effective_cache_size", Type: "size", Default: "4GB", Min: 8 << 10, Max: 1 << 44, Auto: "75% of the memory limit",
		Description: "The planner’s estimate of memory available for caching, including the operating system’s. It allocates nothing."},
	{Name: "work_mem", Type: "size", Default: "4MB", Min: 64 << 10, Max: 16 << 30,
		Description: "Memory per sort or hash step before it spills to disk. One query can use several."},
	{Name: "maintenance_work_mem", Type: "size", Default: "64MB", Min: 1 << 20, Max: 16 << 30, Auto: "1/16 of the memory limit, from 64MB to 2GB",
		Description: "Memory for VACUUM, CREATE INDEX and other maintenance operations, per autovacuum worker."},
	{Name: "random_page_cost", Type: "real", Default: "4", Min: 0, Max: 100,
		Description: "Planner cost of a non-sequential page read. Values near 1.1 suit SSD storage."},
	{Name: "effective_io_concurrency", Type: "integer", Default: "1", Min: 0, Max: 1000,
		Description: "Concurrent disk reads the planner expects the storage to handle. SSDs often use 200."},
	{Name: "default_statistics_target", Type: "integer", Default: "100", Min: 1, Max: 10000,
		Description: "Detail of the statistics ANALYZE collects. Higher values can improve plans for skewed data."},
	{Name: "checkpoint_timeout", Type: "integer", Default: "300", Min: 30, Max: 86400, Unit: "s",
		Description: "Maximum time between automatic WAL checkpoints."},
	{Name: "max_wal_size", Type: "size", Default: "1GB", Min: 32 << 20, Max: 1 << 40,
		Description: "WAL volume that triggers a checkpoint. Larger values smooth out heavy write loads."},
	{Name: "min_wal_size", Type: "size", Default: "80MB", Min: 32 << 20, Max: 1 << 40,
		Description: "WAL volume kept for reuse instead of being removed after a checkpoint."},
	{Name: "checkpoint_completion_target", Type: "real", Default: "0.9", Min: 0, Max: 1,
		Description: "Share of the time between checkpoints over which checkpoint writes are spread, smoothing out I/O."},
	{Name: "wal_compression", Type: "enum", Default: "off", Values: []string{"off", "on", "pglz", "lz4", "zstd"},
		Description: "Compress full-page images in the WAL. lz4 cuts WAL volume under heavy writes for little CPU."},
	{Name: "synchronous_commit", Type: "enum", Default: "on", Values: []string{"on", "local", "off"},
		Description: "off returns from COMMIT before the WAL reaches disk: faster, and a crash can lose the last moments of commits, though never corrupt data."},
	{Name: "autovacuum_max_workers", Type: "integer", Default: "3", Min: 1, Max: 16,
		Description: "Autovacuum processes that run at once. More help databases with many busy tables."},
	{Name: "autovacuum_naptime", Type: "integer", Default: "60", Min: 1, Max: 2147483, Unit: "s",
		Description: "Pause between autovacuum runs on each database."},
	{Name: "autovacuum_vacuum_scale_factor", Type: "real", Default: "0.2", Min: 0, Max: 100,
		Description: "Share of a table that must change before autovacuum cleans it. Lower values, such as 0.05, suit large tables."},
	{Name: "autovacuum_analyze_scale_factor", Type: "real", Default: "0.1", Min: 0, Max: 100,
		Description: "Share of a table that must change before its statistics are refreshed."},
	{Name: "autovacuum_vacuum_cost_limit", Type: "integer", Default: "-1", Min: -1, Max: 10000,
		Description: "Work autovacuum does before it pauses. Higher values, such as 2000, keep up with heavy writes. -1 uses vacuum_cost_limit (200)."},
	{Name: "idle_session_timeout", Type: "integer", Default: "0", Min: 0, Max: 2147483647, Unit: "ms",
		Description: "End sessions that stay idle, outside a transaction, for this long. 0 turns this off."},
	{Name: "track_io_timing", Type: "enum", Default: "off", Values: onOff,
		Description: "Time disk reads and writes for EXPLAIN (ANALYZE, BUFFERS) and pg_stat_statements. Costs a little CPU."},
	{Name: "hash_mem_multiplier", Type: "real", Default: "2", Min: 1, Max: 1000,
		Description: "Multiple of work_mem that hash joins and aggregates may use before spilling to disk."},
	{Name: "log_min_duration_statement", Type: "integer", Default: "-1", Min: -1, Max: 2147483647, Unit: "ms",
		Description: "Log each statement that runs at least this long. 0 logs every statement and -1 turns this off."},
	{Name: "log_statement", Type: "enum", Default: "none", Values: []string{"none", "ddl", "mod", "all"},
		Description: "Statement types to log: schema changes (ddl), data changes as well (mod), or everything."},
	{Name: "log_connections", Type: "enum", Default: "off", Values: onOff,
		Description: "Log each successful client connection."},
	{Name: "log_disconnections", Type: "enum", Default: "off", Values: onOff,
		Description: "Log the end of each session, with its duration."},
	{Name: "statement_timeout", Type: "integer", Default: "0", Min: 0, Max: 2147483647, Unit: "ms",
		Description: "Cancel any statement that runs longer than this. 0 turns the limit off."},
	{Name: "idle_in_transaction_session_timeout", Type: "integer", Default: "0", Min: 0, Max: 2147483647, Unit: "ms",
		Description: "End sessions that stay idle inside an open transaction for this long. 0 turns this off."},
	{Name: "lock_timeout", Type: "integer", Default: "0", Min: 0, Max: 2147483647, Unit: "ms",
		Description: "Cancel a statement that waits longer than this for a lock. 0 waits indefinitely."},
	{Name: "max_parallel_workers_per_gather", Type: "integer", Default: "2", Min: 0, Max: 1024,
		Description: "Parallel workers one query step may start. 0 turns parallel queries off."},
	{Name: "max_worker_processes", Type: "integer", Default: "8", Min: 0, Max: 1024,
		Description: "Background worker processes the server supports, including parallel workers."},
	{Name: "max_parallel_workers", Type: "integer", Default: "8", Min: 0, Max: 1024,
		Description: "Parallel workers all queries may use at once. Keep it at or below the CPU limit."},
	{Name: "max_parallel_maintenance_workers", Type: "integer", Default: "2", Min: 0, Max: 1024,
		Description: "Parallel workers one CREATE INDEX or VACUUM may start."},
	{Name: "jit", Type: "enum", Default: "on", Values: onOff,
		Description: "Compile parts of expensive queries just in time."},
	{Name: "shared_preload_libraries", Type: "list", Default: "pg_stat_statements", Values: []string{"pg_stat_statements", "auto_explain", "pg_prewarm"},
		Description: "Libraries loaded when the server starts. pg_stat_statements, on by default, collects the statement statistics Performance insights shows."},
}

var mysqlParameters = []parameter{
	{Name: "max_connections", Type: "integer", Default: "151", Min: 10, Max: 100000, Unit: "connections",
		Description: "Maximum concurrent client connections."},
	{Name: "innodb_buffer_pool_size", Type: "size", Default: "128M", Min: 5 << 20, Max: 1 << 40, memoryBound: true, Auto: "75% of the memory limit, less 256M for the server",
		Description: "Memory for caching InnoDB data and indexes."},
	{Name: "innodb_redo_log_capacity", Type: "size", Default: "100M", Min: 8 << 20, Max: 128 << 30,
		Description: "Disk space for redo logs. Larger values help sustained write loads."},
	{Name: "innodb_flush_log_at_trx_commit", Type: "enum", Default: "1", Values: []string{"0", "1", "2"},
		Description: "1 writes the log to disk at every commit and is fully durable. 0 and 2 risk about a second of commits for speed."},
	{Name: "sync_binlog", Type: "integer", Default: "1", Min: 0, Max: 4294967295,
		Description: "Commits between binary log syncs to disk. 1 is the safest; 0 leaves syncing to the operating system."},
	{Name: "binlog_expire_logs_seconds", Type: "integer", Default: "2592000", Min: 0, Max: 4294967295, Unit: "s",
		Description: "How long binary logs are kept before automatic removal. Lower values save disk space."},
	{Name: "long_query_time", Type: "real", Default: "10", Min: 0, Max: 31536000, Unit: "s",
		Description: "Queries slower than this are written to the slow query log."},
	{Name: "slow_query_log", Type: "enum", Default: "OFF", Values: onOffSQL,
		Description: "Record slow queries. log_output chooses between a file and the mysql.slow_log table."},
	{Name: "log_output", Type: "enum", Default: "FILE", Values: []string{"FILE", "TABLE", "NONE"},
		Description: "Destination of the slow query and general logs."},
	{Name: "wait_timeout", Type: "integer", Default: "28800", Min: 1, Max: 31536000, Unit: "s",
		Description: "Close non-interactive connections that stay idle for this long."},
	{Name: "interactive_timeout", Type: "integer", Default: "28800", Min: 1, Max: 31536000, Unit: "s",
		Description: "Close interactive client sessions that stay idle for this long."},
	{Name: "max_allowed_packet", Type: "size", Default: "64M", Min: 1 << 10, Max: 1 << 30,
		Description: "Largest packet, and therefore largest row, the server accepts."},
	{Name: "tmp_table_size", Type: "size", Default: "16M", Min: 1 << 10, Max: 1 << 40,
		Description: "Largest in-memory internal temporary table before it moves to disk."},
	{Name: "max_heap_table_size", Type: "size", Default: "16M", Min: 16 << 10, Max: 1 << 40,
		Description: "Largest MEMORY table a user can create."},
	{Name: "table_open_cache", Type: "integer", Default: "4000", Min: 1, Max: 524288,
		Description: "Open tables cached for all threads."},
	{Name: "transaction_isolation", Type: "enum", Default: "REPEATABLE-READ",
		Values:      []string{"READ-UNCOMMITTED", "READ-COMMITTED", "REPEATABLE-READ", "SERIALIZABLE"},
		Description: "Default isolation level for new transactions."},
	{Name: "character_set_server", Type: "enum", Default: "utf8mb4", Values: []string{"utf8mb4", "latin1"},
		Description: "Default character set for new databases and tables."},
	{Name: "sql_mode", Type: "list", Default: "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION",
		Values: []string{"ONLY_FULL_GROUP_BY", "STRICT_TRANS_TABLES", "STRICT_ALL_TABLES", "NO_ZERO_IN_DATE", "NO_ZERO_DATE",
			"ERROR_FOR_DIVISION_BY_ZERO", "NO_ENGINE_SUBSTITUTION", "ANSI_QUOTES", "PIPES_AS_CONCAT", "REAL_AS_FLOAT", "IGNORE_SPACE",
			"NO_AUTO_VALUE_ON_ZERO", "NO_BACKSLASH_ESCAPES", "NO_UNSIGNED_SUBTRACTION", "PAD_CHAR_TO_FULL_LENGTH",
			"TIME_TRUNCATE_FRACTIONAL", "ALLOW_INVALID_DATES", "HIGH_NOT_PRECEDENCE"},
		Description: "SQL behaviour flags. An empty list gives permissive legacy behaviour."},
	{Name: "performance_schema", Type: "enum", Default: "ON", Values: onOffSQL,
		Description: "Collect detailed performance data. Turning it off saves memory on small instances."},
	{Name: "innodb_io_capacity", Type: "integer", Default: "10000", Min: 100, Max: 1000000,
		Description: "Background I/O operations per second InnoDB may use for flushing."},
	{Name: "innodb_io_capacity_max", Type: "integer", Default: "20000", Min: 100, Max: 4000000,
		Description: "I/O operations per second InnoDB may use when flushing falls behind. At least innodb_io_capacity."},
	{Name: "innodb_log_buffer_size", Type: "size", Default: "64M", Min: 1 << 20, Max: 4095 << 20,
		Description: "Memory for redo log writes before they reach disk. Larger values help big transactions."},
	{Name: "innodb_parallel_read_threads", Type: "integer", Default: "4", Min: 1, Max: 256,
		Description: "Threads for parallel clustered index reads, such as CHECK TABLE and SELECT COUNT(*)."},
	{Name: "sort_buffer_size", Type: "size", Default: "256K", Min: 32 << 10, Max: 1 << 30,
		Description: "Memory per sort. Each connection that sorts allocates it, so raise it carefully."},
	{Name: "join_buffer_size", Type: "size", Default: "256K", Min: 1 << 10, Max: 1 << 30,
		Description: "Memory per join that cannot use an index. Each such join allocates it."},
	{Name: "max_execution_time", Type: "integer", Default: "0", Min: 0, Max: 4294967295, Unit: "ms",
		Description: "Stop SELECT statements that run longer than this. 0 turns the limit off."},
	{Name: "innodb_lock_wait_timeout", Type: "integer", Default: "50", Min: 1, Max: 1073741824, Unit: "s",
		Description: "How long a transaction waits for a row lock before giving up."},
}

// Append-only persistence is deliberately absent: Valkey ignores the RDB
// snapshot when AOF is switched on through a restart and starts empty.
var valkeyParameters = []parameter{
	{Name: "maxmemory-policy", Type: "enum", Default: "noeviction",
		Values:      []string{"noeviction", "allkeys-lru", "allkeys-lfu", "allkeys-random", "volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl"},
		Description: "What happens at the memory limit. noeviction refuses writes; the other policies remove keys, which suits a cache. Needs a memory limit."},
	{Name: "save", Type: "schedule", Default: "3600 1 300 100 60 10000",
		Description: "Snapshot rules as pairs of seconds and changes. An empty value turns snapshots off, so data is lost when Valkey restarts."},
	{Name: "timeout", Type: "integer", Default: "0", Min: 0, Max: 2147483647, Unit: "s",
		Description: "Close client connections that stay idle for this long. 0 keeps them open."},
	{Name: "tcp-keepalive", Type: "integer", Default: "300", Min: 0, Max: 2147483647, Unit: "s",
		Description: "Interval of TCP keepalive probes to clients. 0 turns them off."},
	{Name: "maxclients", Type: "integer", Default: "10000", Min: 1, Max: 100000, Unit: "clients",
		Description: "Maximum simultaneous client connections."},
	{Name: "notify-keyspace-events", Type: "flags", Default: "",
		Description: "Keyspace notifications to publish, for example Ex for expired keys. Empty turns them off."},
	{Name: "slowlog-log-slower-than", Type: "integer", Default: "10000", Min: -1, Max: 2147483647, Unit: "µs",
		Description: "Commands slower than this are added to the slow log. 0 logs every command and -1 turns this off."},
	{Name: "slowlog-max-len", Type: "integer", Default: "128", Min: 0, Max: 1000000,
		Description: "Entries kept in the slow log."},
	{Name: "lazyfree-lazy-eviction", Type: "enum", Default: "yes", Values: yesNo,
		Description: "Free evicted keys in a background thread."},
	{Name: "lazyfree-lazy-expire", Type: "enum", Default: "yes", Values: yesNo,
		Description: "Free expired keys in a background thread."},
	{Name: "lazyfree-lazy-server-del", Type: "enum", Default: "yes", Values: yesNo,
		Description: "Free values replaced or deleted by the server in a background thread."},
	{Name: "lazyfree-lazy-user-del", Type: "enum", Default: "yes", Values: yesNo,
		Description: "Make DEL free values in a background thread, like UNLINK."},
	{Name: "hz", Type: "integer", Default: "10", Min: 1, Max: 500,
		Description: "Frequency of background tasks such as expiring keys. Higher values use more CPU."},
	{Name: "io-threads", Type: "integer", Default: "1", Min: 1, Max: 64,
		Description: "Threads for network I/O. More than one helps busy instances that have several CPU cores."},
	{Name: "maxmemory-samples", Type: "integer", Default: "5", Min: 1, Max: 64,
		Description: "Keys sampled for each eviction. Higher values approximate LRU and LFU more closely."},
	{Name: "maxmemory-eviction-tenacity", Type: "integer", Default: "10", Min: 0, Max: 100,
		Description: "Effort spent evicting keys per command. Raise it when heavy writes keep memory above the limit."},
	{Name: "activedefrag", Type: "enum", Default: "no", Values: yesNo,
		Description: "Defragment memory in the background, so long-running caches with churning keys return memory. Uses some CPU."},
	{Name: "latency-monitor-threshold", Type: "integer", Default: "0", Min: 0, Max: 2147483647, Unit: "ms",
		Description: "Record events slower than this for the LATENCY command. 0 turns monitoring off."},
}

func number(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// Memory settings sized to the instance's memory limit, as RDS sizes them to
// the instance class, so a larger instance caches more without tuning. A
// parameter the user sets wins. Without a memory limit the engine defaults
// stay: OpenRDS cannot know how much of the host a database may use.
func tunedParameters(engine string, memoryMiB int) map[string]string {
	if memoryMiB <= 0 {
		return nil
	}
	switch engine {
	case "postgres":
		return map[string]string{
			"shared_buffers":       pgSize(memoryMiB / 4),
			"effective_cache_size": pgSize(memoryMiB * 3 / 4),
			"maintenance_work_mem": pgSize(min(max(memoryMiB/16, 64), 2048)),
		}
	case "mysql":
		pool, instances := bufferPool(memoryMiB)
		return map[string]string{"innodb_buffer_pool_size": strconv.Itoa(pool) + "M", "innodb_buffer_pool_instances": strconv.Itoa(instances)}
	}
	return nil
}

func pgSize(mib int) string {
	if mib%1024 == 0 {
		return strconv.Itoa(mib/1024) + "GB"
	}
	return strconv.Itoa(mib) + "MB"
}

// Three quarters of the memory limit, less 256 MiB for the server's own
// memory (connections, performance_schema), in one pool instance per GiB up
// to 8. MySQL rounds the pool up to whole chunks of 128 MiB times the
// instances, and its default instance count depends on the version and the
// host's CPUs, so OpenRDS sets both and rounds down: rounding up could pass
// the memory limit.
func bufferPool(memoryMiB int) (poolMiB, instances int) {
	pool := max(memoryMiB*3/4-256, 128)
	instances = min(max(pool/1024, 1), 8)
	step := 128 * instances
	return max(pool/step*step, 128), instances
}

// Settings OpenRDS changes from the engine's own default, as RDS's default
// parameter groups do: PostgreSQL loads pg_stat_statements for Performance
// insights. Its overhead is small.
var defaultParameters = map[string]map[string]string{
	"postgres": {"shared_preload_libraries": "pg_stat_statements"},
}

// The settings the server runs with: OpenRDS's defaults, the memory-sized
// ones, then the user's.
func (r record) effectiveParameters() map[string]string {
	p := maps.Clone(defaultParameters[r.Engine])
	if p == nil {
		p = make(map[string]string, len(r.Parameters))
	}
	maps.Copy(p, tunedParameters(r.Engine, r.MemoryMiB))
	if _, ok := r.Parameters["innodb_buffer_pool_size"]; ok {
		delete(p, "innodb_buffer_pool_instances") // sized for OpenRDS's pool, not the user's
	}
	maps.Copy(p, r.Parameters)
	return p
}

// Size in bytes of a value such as 256MB (PostgreSQL) or 256M (MySQL).
func sizeBytes(engine, value string) (int64, bool) {
	for i, unit := range sizeUnits[engine] {
		digits, ok := strings.CutSuffix(value, unit)
		if !ok || !digitsPattern.MatchString(digits) {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		shift := 10 * (i + 1)
		if err != nil || n > math.MaxInt64>>shift {
			return 0, false
		}
		return n << shift, true
	}
	return 0, false
}

func (p parameter) check(engine, value string) error {
	bad := func(rule string) error { return fail(400, p.Name+": "+rule+".") }
	between := fmt.Sprintf("from %s to %s", number(p.Min), number(p.Max))
	switch p.Type {
	case "integer":
		n, err := strconv.ParseInt(value, 10, 64)
		if !integerPattern.MatchString(value) || err != nil || float64(n) < p.Min || float64(n) > p.Max {
			return bad("enter a whole number " + between)
		}
	case "real":
		f, err := strconv.ParseFloat(value, 64)
		if !realPattern.MatchString(value) || err != nil || f < p.Min || f > p.Max {
			return bad("enter a number " + between)
		}
	case "size":
		n, ok := sizeBytes(engine, value)
		if !ok {
			return bad("enter a whole number followed by one of " + strings.Join(sizeUnits[engine], ", "))
		}
		if float64(n) < p.Min || float64(n) > p.Max {
			return bad(fmt.Sprintf("choose a size from %s to %s", bytesLabel(int64(p.Min)), bytesLabel(int64(p.Max))))
		}
	case "enum":
		if !slices.Contains(p.Values, value) {
			return bad("choose one of " + strings.Join(p.Values, ", "))
		}
	case "list":
		if value == "" {
			return nil
		}
		items := strings.Split(value, ",")
		for i, item := range items {
			if !slices.Contains(p.Values, item) || slices.Contains(items[:i], item) {
				return bad("use a comma-separated list of " + strings.Join(p.Values, ", "))
			}
		}
	case "flags":
		if !flagsPattern.MatchString(value) {
			return bad("use the letters K E g $ l s h z x e t m d n A")
		}
	case "schedule":
		if !schedulePattern.MatchString(value) {
			return bad("use pairs of seconds and changes, such as 3600 1 300 100, or leave it empty")
		}
	}
	return nil
}

func bytesLabel(n int64) string {
	units := []string{"bytes", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for n >= 1024 && n%1024 == 0 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return strconv.FormatInt(n, 10) + " " + units[i]
}

func checkParameters(engine string, memoryMiB int, params map[string]string) error {
	catalog := engines[engine].parameters
	for _, name := range slices.Sorted(maps.Keys(params)) {
		i := slices.IndexFunc(catalog, func(p parameter) bool { return p.Name == name })
		if i < 0 {
			return fail(400, fmt.Sprintf("%s is not an adjustable %s parameter.", name, engines[engine].title))
		}
		p := catalog[i]
		if err := p.check(engine, params[name]); err != nil {
			return err
		}
		if n, _ := sizeBytes(engine, params[name]); p.memoryBound && memoryMiB > 0 && n >= int64(memoryMiB)<<20 {
			return fail(400, fmt.Sprintf("%s must be smaller than the %d MiB memory limit.", name, memoryMiB))
		}
	}
	return nil
}

// Where Compose places the injected files in the database container.
const (
	tlsCertFile = "/run/openrds/server.crt"
	tlsKeyFile  = "/run/openrds/server.key"
	tlsCAFile   = "/run/openrds/ca.crt"
)

// The database command line. Compose interpolates the override file, so
// user values escape "$".
func (r record) command() []string {
	escape := func(v string) string { return strings.ReplaceAll(v, "$", "$$") }
	params := r.effectiveParameters()
	keys := slices.Sorted(maps.Keys(params))
	var cmd []string
	switch r.Engine {
	case "postgres":
		cmd = []string{"postgres", "-c", "ssl=on", "-c", "ssl_cert_file=" + tlsCertFile, "-c", "ssl_key_file=" + tlsKeyFile,
			"-c", "hba_file=/run/openrds/pg_hba.conf"}
		for _, k := range keys {
			cmd = append(cmd, "-c", k+"="+escape(params[k]))
		}
	case "mysql":
		cmd = []string{"mysqld", "--ssl-ca=" + tlsCAFile, "--ssl-cert=" + tlsCertFile, "--ssl-key=" + tlsKeyFile}
		if r.RequireTLS {
			cmd = append(cmd, "--require-secure-transport=ON")
		}
		for _, k := range keys {
			cmd = append(cmd, "--"+k+"="+escape(params[k]))
		}
	case "valkey":
		policy := "noeviction"
		if v, ok := r.Parameters["maxmemory-policy"]; ok {
			policy = v
		}
		// Users, including the owner as "default", come from the injected file.
		cmd = []string{"valkey-server", "--include", "/run/openrds/users.conf", "--dir", "/data",
			"--maxmemory", "${OPENRDS_VALKEY_MAXMEMORY:-0}", "--maxmemory-policy", policy}
		if r.RequireTLS {
			// Valkey serves TLS or plain text on a port, so TLS replaces plain text.
			cmd = append(cmd, "--port", "0", "--tls-port", "6379", "--tls-cert-file", tlsCertFile, "--tls-key-file", tlsKeyFile,
				"--tls-ca-cert-file", tlsCAFile, "--tls-auth-clients", "no")
		}
		for _, k := range keys {
			if k != "maxmemory-policy" {
				cmd = append(cmd, "--"+k, escape(r.Parameters[k]))
			}
		}
	}
	return cmd
}

func (m *manager) overridePath(r record) string {
	return filepath.Join(m.dir, "compose", r.Project+".json")
}

// A generated Compose file (JSON is YAML) with the command and a label that
// changes with the injected files, so Compose recreates the container then.
func (m *manager) writeOverride(r record, secrets, poolerSecrets string) (string, error) {
	path := m.overridePath(r)
	services := map[string]any{"db": map[string]any{
		"command": r.command(),
		"labels":  map[string]string{"io.openrds.secrets": secrets},
	}}
	if r.Pooler.on() {
		services["pooler"] = map[string]any{"labels": map[string]string{"io.openrds.secrets": poolerSecrets}}
	}
	doc := map[string]any{"services": services}
	if r.DataGeneration > 0 {
		doc["volumes"] = map[string]any{"data": map[string]any{"name": r.dataVolume()}}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return path, nil
	}
	return path, writeFile(path, data)
}
