package main

// Performance insights, like RDS Performance Insights: what each engine
// reports about itself — connections, throughput, cache hit ratio, the
// statements that take the most time, what runs now, and Valkey's slow log.
// The standard library has no database drivers, so OpenRDS runs the engine's
// own client inside the container, as it does for database users. Its
// statements carry /* openrds */ after their first word, where PostgreSQL
// keeps it in the statement text (MySQL's run in the mysql schema), so the
// statistics it shows leave them out.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Cumulative engine counters from one reading.
type dbSample struct {
	at                      time.Time
	conns, active, maxConns int
	ops                     uint64 // transactions (PostgreSQL), statements (MySQL) or commands (Valkey)
	hits, misses            uint64 // reads served from memory, and reads that missed it
	activeKnown             bool   // Valkey has no equivalent of running statements
}

// What a chart shows from the engine: rates cover the time since the previous reading.
type dbPoint struct {
	Conns    float64  `json:"conns"`
	Active   *float64 `json:"active,omitempty"`
	MaxConns int      `json:"maxConns,omitempty"`
	Ops      float64  `json:"ops"`           // per second
	Hit      *float64 `json:"hit,omitempty"` // percent of reads served from memory, when there were reads
}

const (
	pgClient    = `exec psql -U openrds -d app -AtX -v ON_ERROR_STOP=1`
	mysqlClient = `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -u root -N -B -r -D mysql`
	// The first input line is the password, as for database users.
	valkeyClient = `IFS= read -r REDISCLI_AUTH; export REDISCLI_AUTH; exec valkey-cli --no-auth-warning $OPENRDS_CLI_FLAGS`
)

const pgSampleSQL = `SELECT /* openrds */
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') - 1,
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state = 'active') - 1,
  current_setting('max_connections'),
  sum(xact_commit + xact_rollback), sum(blks_hit), sum(blks_read)
FROM pg_stat_database;
`

// Threads_running counts background threads too, so running statements come
// from the process list.
const mysqlSampleSQL = `SHOW GLOBAL STATUS WHERE Variable_name IN
  ('Threads_connected', 'Questions', 'Innodb_buffer_pool_read_requests', 'Innodb_buffer_pool_reads');
SELECT 'max_connections', @@max_connections;
SELECT 'active', COUNT(*) FROM information_schema.PROCESSLIST
  WHERE COMMAND NOT IN ('Sleep', 'Daemon', 'Binlog Dump') AND ID <> CONNECTION_ID();
`

// Run the engine's client in a container with input on stdin, so passwords
// and SQL stay off command lines.
func (m *manager) engineClient(ctx context.Context, container, script, input string) (string, error) {
	var out bytes.Buffer
	err := m.docker.run(ctx, nil, strings.NewReader(input), &out, "exec", "-i", container, "sh", "-ec", script)
	return out.String(), err
}

func uint64s(fields ...string) ([]uint64, bool) {
	out := make([]uint64, len(fields))
	for i, f := range fields {
		n, err := strconv.ParseFloat(strings.TrimSpace(f), 64) // sum() over bigint is numeric
		if err != nil || n < 0 {
			return nil, false
		}
		out[i] = uint64(n)
	}
	return out, true
}

// Read the engine's counters. A database that is starting or overloaded
// may not answer; the reading is then skipped.
func (m *manager) sampleEngine(ctx context.Context, r record, container string) (*dbSample, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s := &dbSample{at: time.Now()}
	switch r.Engine {
	case "postgres":
		out, err := m.engineClient(ctx, container, pgClient, pgSampleSQL)
		if err != nil {
			return nil, err
		}
		f := strings.Split(strings.TrimSpace(out), "|")
		n, ok := uint64s(f...)
		if len(f) != 6 || !ok {
			return nil, fmt.Errorf("unexpected PostgreSQL reading %q", out)
		}
		s.conns, s.active, s.maxConns, s.ops, s.hits, s.misses = int(n[0]), int(n[1]), int(n[2]), n[3], n[4], n[5]
		s.activeKnown = true
	case "mysql":
		out, err := m.engineClient(ctx, container, mysqlClient, mysqlSampleSQL)
		if err != nil {
			return nil, err
		}
		v := map[string]uint64{}
		for _, line := range strings.Split(out, "\n") {
			if name, value, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
				if n, ok := uint64s(value); ok {
					v[strings.ToLower(name)] = n[0]
				}
			}
		}
		if _, ok := v["questions"]; !ok {
			return nil, fmt.Errorf("unexpected MySQL reading %q", out)
		}
		requests, reads := v["innodb_buffer_pool_read_requests"], v["innodb_buffer_pool_reads"]
		s.conns, s.active, s.maxConns = int(max(v["threads_connected"], 1)-1), int(v["active"]), int(v["max_connections"])
		s.ops, s.hits, s.misses = v["questions"], requests-min(reads, requests), reads
		s.activeKnown = true
	case "valkey":
		out, err := m.engineClient(ctx, container, valkeyClient, r.Password+"\nINFO\n")
		if err != nil {
			return nil, err
		}
		v := map[string]uint64{}
		for _, line := range strings.Split(out, "\n") {
			if name, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
				if n, ok := uint64s(value); ok {
					v[name] = n[0]
				}
			}
		}
		if _, ok := v["total_commands_processed"]; !ok {
			return nil, errors.New("unexpected Valkey reading")
		}
		s.conns, s.maxConns = int(max(v["connected_clients"], 1)-1), int(v["maxclients"])
		s.ops, s.hits, s.misses = v["total_commands_processed"], v["keyspace_hits"], v["keyspace_misses"]
	default:
		return nil, nil
	}
	return s, nil
}

// The chart values between two readings of the same container.
func engineRates(prev, cur *dbSample) *dbPoint {
	p := &dbPoint{Conns: float64(cur.conns), MaxConns: cur.maxConns}
	if cur.activeKnown {
		a := float64(max(cur.active, 0))
		p.Active = &a
	}
	if prev == nil {
		return p
	}
	seconds := cur.at.Sub(prev.at).Seconds()
	if seconds > 0 && cur.ops >= prev.ops {
		p.Ops = float64(cur.ops-prev.ops) / seconds
	}
	if cur.hits >= prev.hits && cur.misses >= prev.misses {
		if hits, misses := cur.hits-prev.hits, cur.misses-prev.misses; hits+misses > 0 {
			hit := float64(hits) / float64(hits+misses) * 100
			p.Hit = &hit
		}
	}
	return p
}

// ---------- insights: statements, sessions and the slow log

type statement struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	TotalMs float64 `json:"totalMs"`
	MeanMs  float64 `json:"meanMs"`
	Rows    int64   `json:"rows"`
}

type session struct {
	ID      string  `json:"id"`
	User    string  `json:"user,omitempty"`
	State   string  `json:"state,omitempty"`
	Wait    string  `json:"wait,omitempty"`
	Seconds float64 `json:"seconds"`
	Query   string  `json:"query,omitempty"`
}

type slowEntry struct {
	At      time.Time `json:"at"`
	Micros  int64     `json:"micros"`
	Command string    `json:"command"`
	Client  string    `json:"client,omitempty"`
}

type insightsView struct {
	Engine string `json:"engine"`
	// Statements by total time; null when the engine does not collect them,
	// with StatementsNote saying how to turn them on.
	Statements     []statement `json:"statements"`
	StatementsNote string      `json:"statementsNote,omitempty"`
	Running        []session   `json:"running"`
	Slow           []slowEntry `json:"slow,omitempty"`
}

const insightRows = 25

// pg_stat_statements needs its library at server start, which OpenRDS
// loads by default, and the extension in the database, created here.
const pgInsightsSQL = `SELECT /* openrds */ current_setting('shared_preload_libraries') LIKE '%pg_stat_statements%' AS loaded \gset
\if :loaded
CREATE /* openrds */ EXTENSION IF NOT EXISTS pg_stat_statements;
SELECT /* openrds */ coalesce(array_to_json(array_agg(t)), '[]') FROM (
  SELECT left(query, 2000) AS query, calls, round(total_exec_time::numeric, 3) AS "totalMs",
         round(mean_exec_time::numeric, 3) AS "meanMs", rows
  FROM pg_stat_statements WHERE query NOT LIKE '%/* openrds */%' ORDER BY total_exec_time DESC LIMIT 25) t;
\else
\echo off
\endif
SELECT /* openrds */ coalesce(array_to_json(array_agg(t)), '[]') FROM (
  SELECT pid::text AS id, usename AS "user", state, nullif(concat_ws(': ', wait_event_type, wait_event), '') AS wait,
         round(extract(epoch FROM now() - coalesce(query_start, xact_start, backend_start))::numeric, 1)::float AS seconds,
         left(query, 2000) AS query
  FROM pg_stat_activity
  WHERE backend_type = 'client backend' AND state <> 'idle' AND pid <> pg_backend_pid() AND query NOT LIKE '%/* openrds */%'
  ORDER BY seconds DESC LIMIT 25) t;
`

// OpenRDS runs in the mysql schema, so its own digests are left out, as is
// the version query the mysql client sends when it connects.
const mysqlInsightsSQL = `SELECT @@performance_schema;
SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('query', LEFT(DIGEST_TEXT, 2000), 'calls', COUNT_STAR,
  'totalMs', ROUND(SUM_TIMER_WAIT / 1e9, 3), 'meanMs', ROUND(AVG_TIMER_WAIT / 1e9, 3), 'rows', SUM_ROWS_SENT)), JSON_ARRAY())
FROM (SELECT * FROM performance_schema.events_statements_summary_by_digest
  WHERE DIGEST_TEXT IS NOT NULL AND (SCHEMA_NAME IS NULL OR SCHEMA_NAME <> 'mysql')
    AND DIGEST_TEXT NOT LIKE 'SELECT @@_version_comment_ LIMIT%' ORDER BY SUM_TIMER_WAIT DESC LIMIT 25) d;
SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('id', CAST(ID AS CHAR), 'user', USER, 'state', COMMAND, 'wait', STATE,
  'seconds', TIME, 'query', LEFT(INFO, 2000))), JSON_ARRAY())
FROM information_schema.PROCESSLIST WHERE COMMAND NOT IN ('Sleep', 'Daemon', 'Binlog Dump') AND ID <> CONNECTION_ID();
`

func (m *manager) runningContainer(ctx context.Context, r record) (string, error) {
	var out bytes.Buffer
	if err := m.compose(ctx, r, nil, &out, "ps", "--quiet", "--status", "running", "db"); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if !containerIDPattern.MatchString(id) {
		return "", fail(409, "Start the database to see its performance.")
	}
	return id, nil
}

func (m *manager) insights(ctx context.Context, name string) (insightsView, error) {
	r, err := m.record(name)
	if err != nil {
		return insightsView{}, err
	}
	v := insightsView{Engine: r.Engine, Running: []session{}}
	id, err := m.runningContainer(ctx, r)
	if err != nil {
		return v, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unreadable := func(err error) error {
		return fail(502, "The database did not answer: "+strings.TrimPrefix(err.Error(), "docker: "))
	}
	switch r.Engine {
	case "postgres":
		out, err := m.engineClient(ctx, id, pgClient, pgInsightsSQL)
		if err != nil {
			return v, unreadable(err)
		}
		lines := nonEmptyLines(out)
		if len(lines) < 2 {
			return v, fail(502, "The database returned an unexpected answer.")
		}
		if lines[0] == "off" {
			v.StatementsNote = "Statement statistics are off. Add pg_stat_statements to shared_preload_libraries in Configuration → Engine parameters, then reboot."
		} else if err := json.Unmarshal([]byte(lines[len(lines)-2]), &v.Statements); err != nil {
			return v, fail(502, "The statement statistics could not be read.")
		}
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &v.Running); err != nil {
			return v, fail(502, "The running statements could not be read.")
		}
	case "mysql":
		out, err := m.engineClient(ctx, id, mysqlClient, mysqlInsightsSQL)
		if err != nil {
			return v, unreadable(err)
		}
		lines := nonEmptyLines(out)
		if len(lines) != 3 {
			return v, fail(502, "The database returned an unexpected answer.")
		}
		if lines[0] != "1" {
			v.StatementsNote = "Statement statistics need performance_schema. Turn it on in Configuration → Engine parameters, then reboot."
		} else if err := json.Unmarshal([]byte(lines[1]), &v.Statements); err != nil {
			return v, fail(502, "The statement statistics could not be read.")
		}
		if err := json.Unmarshal([]byte(lines[2]), &v.Running); err != nil {
			return v, fail(502, "The running statements could not be read.")
		}
	case "valkey":
		// OpenRDS's own commands are filtered out afterwards, so read well past them.
		out, err := m.engineClient(ctx, id, valkeyClient+" --json SLOWLOG GET 512", r.Password+"\n")
		if err != nil {
			return v, unreadable(err)
		}
		if v.Slow, err = parseSlowlog(out); err != nil {
			return v, fail(502, "The slow log could not be read.")
		}
		v.StatementsNote = "Valkey keeps a slow log instead of statement statistics: commands slower than slowlog-log-slower-than (10 ms by default)."
	}
	slices.SortFunc(v.Statements, func(a, b statement) int { return cmpFloat(b.TotalMs, a.TotalMs) })
	slices.SortFunc(v.Running, func(a, b session) int { return cmpFloat(b.Seconds, a.Seconds) })
	v.Running = v.Running[:min(len(v.Running), insightRows)]
	return v, nil
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Entries of SLOWLOG GET as valkey-cli --json prints them, newest first.
// Commands from inside the container (OpenRDS and the health check) are
// left out: clients reach Valkey over the network.
func parseSlowlog(out string) ([]slowEntry, error) {
	var raw [][]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return nil, err
	}
	entries := []slowEntry{}
	for _, e := range raw {
		if len(e) < 4 {
			continue
		}
		var at, micros int64
		var args []string
		var client string
		if json.Unmarshal(e[1], &at) != nil || json.Unmarshal(e[2], &micros) != nil || json.Unmarshal(e[3], &args) != nil || len(args) == 0 {
			continue
		}
		if len(e) > 4 {
			json.Unmarshal(e[4], &client)
		}
		if strings.HasPrefix(client, "127.0.0.1:") || strings.HasPrefix(client, "[::1]:") {
			continue
		}
		entries = append(entries, slowEntry{At: time.Unix(at, 0).UTC(), Micros: micros, Command: strings.Join(args, " "), Client: client})
		if len(entries) == insightRows {
			break
		}
	}
	return entries, nil
}

var sessionIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// Cancel the statement a session is running, leaving its connection open.
func (m *manager) cancelStatement(ctx context.Context, name, session string) error {
	r, err := m.record(name)
	if err != nil {
		return err
	}
	if !sessionIDPattern.MatchString(session) {
		return fail(400, "Choose a session from the list.")
	}
	id, err := m.runningContainer(ctx, r)
	if err != nil {
		return err
	}
	var script, sql string
	switch r.Engine {
	case "postgres":
		script, sql = pgClient, "SELECT /* openrds */ pg_cancel_backend("+session+");\n"
	case "mysql":
		script, sql = mysqlClient, "KILL QUERY "+session+";\n"
	default:
		return fail(400, "Valkey runs one command at a time; there is nothing to cancel.")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := m.engineClient(ctx, id, script, sql)
	if err == nil && r.Engine == "postgres" && strings.TrimSpace(out) != "t" {
		err = errors.New("the session has already finished")
	}
	if err != nil {
		m.note(ctx, name, "Cancel statement", "failed", "Session "+session+": "+strings.TrimPrefix(err.Error(), "docker: "))
		return fail(409, "The statement was not cancelled: "+strings.TrimPrefix(err.Error(), "docker: "))
	}
	m.note(ctx, name, "Cancel statement", "completed", "Session "+session)
	return nil
}

// Start the statistics afresh, for example after adding an index.
func (m *manager) resetInsights(ctx context.Context, name string) error {
	r, err := m.record(name)
	if err != nil {
		return err
	}
	id, err := m.runningContainer(ctx, r)
	if err != nil {
		return err
	}
	var script, input string
	switch r.Engine {
	case "postgres":
		script, input = pgClient, "SELECT /* openrds */ pg_stat_statements_reset();\n"
	case "mysql":
		script, input = mysqlClient, "TRUNCATE TABLE performance_schema.events_statements_summary_by_digest;\n"
	case "valkey":
		script, input = valkeyClient, r.Password+"\nSLOWLOG RESET\n"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := m.engineClient(ctx, id, script, input); err != nil {
		m.note(ctx, name, "Reset query statistics", "failed", strings.TrimPrefix(err.Error(), "docker: "))
		return fail(409, "The statistics were not reset: "+strings.TrimPrefix(err.Error(), "docker: "))
	}
	m.note(ctx, name, "Reset query statistics", "completed", "")
	return nil
}

// Statement text can hold data, so insights need the operator role, like
// connection details; charts of the counters are open to viewers.
func insightRoutes(add func(string, int, endpoint), m *manager) {
	add("GET /api/instances/{name}/insights", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		v, err := m.insights(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 200, v)
		}
		return err
	})
	add("POST /api/instances/{name}/insights/reset", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.resetInsights(r.Context(), r.PathValue("name")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("POST /api/instances/{name}/sessions/{id}/cancel", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.cancelStatement(r.Context(), r.PathValue("name"), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
