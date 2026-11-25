package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A container whose engine client answers with canned output, by engine.
type engineFake struct {
	mu      sync.Mutex
	answers map[string]string // a word in the script or input → output
	fail    bool
	scripts []string
	inputs  []string
	args    [][]string
}

func (f *engineFake) run(_ context.Context, _ []string, in io.Reader, out io.Writer, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args[0] == "compose" && composeVerb(args) == "ps" {
		fmt.Fprintln(out, "abcdef123456")
		return nil
	}
	if args[0] != "exec" {
		return nil
	}
	f.args = append(f.args, args)
	data, _ := io.ReadAll(in)
	script := args[len(args)-1]
	f.scripts, f.inputs = append(f.scripts, script), append(f.inputs, string(data))
	if f.fail {
		return fmt.Errorf("docker: the database system is starting up")
	}
	for key, answer := range f.answers {
		if strings.Contains(script+string(data), key) {
			fmt.Fprint(out, answer)
			return nil
		}
	}
	return nil
}

func engineManager(t *testing.T, f *engineFake, engine string) (*manager, record) {
	t.Helper()
	m := testManager(t, f.run)
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: engine, Port: 15432}, Project: "openrds-db-orders", Password: "s3cret-password"}
	if err := writeJSON(m.recordPath(r.Name), r); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestEngineReadingsBecomeRates(t *testing.T) {
	t0 := time.Now()
	prev := &dbSample{at: t0, conns: 4, active: 1, maxConns: 100, ops: 1000, hits: 900, misses: 100, activeKnown: true}
	cur := &dbSample{at: t0.Add(30 * time.Second), conns: 6, active: 2, maxConns: 100, ops: 1600, hits: 1890, misses: 110, activeKnown: true}
	p := engineRates(prev, cur)
	if p.Conns != 6 || *p.Active != 2 || p.MaxConns != 100 || p.Ops != 20 || p.Hit == nil || *p.Hit != 99 {
		t.Fatalf("rates: %+v hit %v", p, *p.Hit)
	}
	if first := engineRates(nil, cur); first.Ops != 0 || first.Hit != nil {
		t.Fatal("the first reading of a container has no rates")
	}
	restarted := &dbSample{at: t0.Add(60 * time.Second), ops: 5, hits: 3, misses: 1}
	if p := engineRates(cur, restarted); p.Ops != 0 || p.Hit != nil || p.Active != nil {
		t.Fatalf("counters that restart with the server read as no traffic: %+v", p)
	}
	quiet := &dbSample{at: t0.Add(60 * time.Second), ops: 1600, hits: 1890, misses: 110}
	if p := engineRates(cur, quiet); p.Hit != nil {
		t.Fatal("no reads means no hit ratio, not 0%")
	}

	// Chart buckets average only the readings the engine answered.
	var avg dbAverage
	hit := 90.0
	avg.add(&dbPoint{Conns: 10, Ops: 100, Hit: &hit, MaxConns: 100})
	avg.add(nil)
	avg.add(&dbPoint{Conns: 20, Ops: 300, MaxConns: 100})
	got := avg.point()
	if got.Conns != 15 || got.Ops != 200 || *got.Hit != 90 || got.Active != nil {
		t.Fatalf("average: %+v", got)
	}
	if (&dbAverage{}).point() != nil {
		t.Fatal("a bucket without readings has no engine point")
	}
}

func TestEachEngineIsReadThroughItsOwnClient(t *testing.T) {
	cases := []struct {
		engine, answer string
		want           dbSample
	}{
		{"postgres", "5|2|100|123456|9900|100\n", dbSample{conns: 5, active: 2, maxConns: 100, ops: 123456, hits: 9900, misses: 100, activeKnown: true}},
		{"mysql", "Innodb_buffer_pool_read_requests\t5000\nInnodb_buffer_pool_reads\t50\nQuestions\t777\nThreads_connected\t8\nmax_connections\t151\nactive\t2\n",
			dbSample{conns: 7, active: 2, maxConns: 151, ops: 777, hits: 4950, misses: 50, activeKnown: true}},
		{"valkey", "# Clients\r\nconnected_clients:4\r\nmaxclients:10000\r\n# Stats\r\ntotal_commands_processed:4242\r\nkeyspace_hits:80\r\nkeyspace_misses:20\r\n",
			dbSample{conns: 3, maxConns: 10000, ops: 4242, hits: 80, misses: 20}},
	}
	for _, c := range cases {
		f := &engineFake{answers: map[string]string{"": c.answer}}
		m, r := engineManager(t, f, c.engine)
		got, err := m.sampleEngine(t.Context(), r, "abcdef123456")
		if err != nil {
			t.Fatalf("%s: %v", c.engine, err)
		}
		got.at = time.Time{}
		if *got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.engine, *got, c.want)
		}
		for _, a := range f.args {
			if strings.Contains(strings.Join(a, " "), r.Password) {
				t.Errorf("%s: the password reached a command line", c.engine)
			}
		}
		if c.engine == "valkey" && !strings.HasPrefix(f.inputs[0], r.Password+"\n") {
			t.Error("Valkey reads its password from the first input line")
		}
	}
	f := &engineFake{fail: true}
	m, r := engineManager(t, f, "postgres")
	if s, err := m.sampleEngine(t.Context(), r, "abcdef123456"); err == nil || s != nil {
		t.Fatal("a database that does not answer gives no reading")
	}
}

func TestInsightsShowStatementsSessionsAndTheSlowLog(t *testing.T) {
	pg := &engineFake{answers: map[string]string{"pg_stat_statements": `CREATE EXTENSION
[{"query":"SELECT * FROM orders WHERE id = $1","calls":10,"totalMs":5.5,"meanMs":0.55,"rows":10},{"query":"UPDATE stock SET n = n - $1","calls":3,"totalMs":90,"meanMs":30,"rows":3}]
[{"id":"4242","user":"app","state":"active","wait":"Lock: transactionid","seconds":12.5,"query":"UPDATE stock SET n = n - 1"}]
`}}
	m, _ := engineManager(t, pg, "postgres")
	v, err := m.insights(t.Context(), "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Statements) != 2 || v.Statements[0].TotalMs != 90 || v.StatementsNote != "" {
		t.Fatalf("statements by total time: %+v", v)
	}
	if len(v.Running) != 1 || v.Running[0].Wait != "Lock: transactionid" || v.Running[0].Seconds != 12.5 {
		t.Fatalf("running: %+v", v.Running)
	}
	pg.answers = map[string]string{"pg_stat_statements": "off\n[]\n"}
	if v, err := m.insights(t.Context(), "orders"); err != nil || v.Statements != nil || !strings.Contains(v.StatementsNote, "shared_preload_libraries") {
		t.Fatalf("statistics off: %+v %v", v, err)
	}

	my := &engineFake{answers: map[string]string{"performance_schema": `0
[]
[{"id":"17","user":"app","state":"Query","wait":"Sending data","seconds":40,"query":"SELECT SLEEP(100)"}]
`}}
	m, _ = engineManager(t, my, "mysql")
	if v, err := m.insights(t.Context(), "orders"); err != nil || !strings.Contains(v.StatementsNote, "performance_schema") || len(v.Running) != 1 {
		t.Fatalf("MySQL without performance_schema: %+v %v", v, err)
	}

	vk := &engineFake{answers: map[string]string{"SLOWLOG": `[[9,1795622331,25000,["KEYS","*"],"10.0.0.5:5000",""],[8,1795622330,13,["HELLO","3"],"127.0.0.1:41000",""],[7,1795622329,12000,["PING"],"[::1]:5000",""]]`}}
	m, _ = engineManager(t, vk, "valkey")
	v, err = m.insights(t.Context(), "orders")
	if err != nil || len(v.Slow) != 1 || v.Slow[0].Command != "KEYS *" || v.Slow[0].Micros != 25000 || v.Slow[0].Client != "10.0.0.5:5000" {
		t.Fatalf("slow log without commands from inside the container: %+v %v", v.Slow, err)
	}
}

func TestCancellingAStatement(t *testing.T) {
	pg := &engineFake{answers: map[string]string{"pg_cancel_backend": "t\n"}}
	m, _ := engineManager(t, pg, "postgres")
	for _, bad := range []string{"0", "12; DROP TABLE x", "abc", "-1"} {
		if err := m.cancelStatement(t.Context(), "orders", bad); status(err) != 400 {
			t.Errorf("session %q: %v", bad, err)
		}
	}
	if err := m.cancelStatement(t.Context(), "orders", "4242"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pg.inputs[len(pg.inputs)-1], "pg_cancel_backend(4242)") {
		t.Fatalf("cancel: %q", pg.inputs)
	}
	pg.answers["pg_cancel_backend"] = "f\n"
	if err := m.cancelStatement(t.Context(), "orders", "4242"); status(err) != 409 {
		t.Fatal("a finished session must be reported")
	}
	items, _ := m.readActivity()
	if items[0].Action != "Cancel statement" || items[0].Result != "failed" || items[1].Result != "completed" {
		t.Fatalf("cancels are recorded: %+v", items[:2])
	}
	vk := &engineFake{}
	m, _ = engineManager(t, vk, "valkey")
	if err := m.cancelStatement(t.Context(), "orders", "1"); status(err) != 400 {
		t.Fatal("Valkey has nothing to cancel")
	}
}

func TestPostgresCollectsStatementStatisticsByDefault(t *testing.T) {
	cmd := strings.Join(record{instanceSpec: instanceSpec{Engine: "postgres"}}.command(), " ")
	if !strings.Contains(cmd, "-c shared_preload_libraries=pg_stat_statements") {
		t.Fatalf("command: %s", cmd)
	}
	none := strings.Join(record{instanceSpec: instanceSpec{Engine: "postgres", Parameters: map[string]string{"shared_preload_libraries": ""}}}.command(), " ")
	if !strings.Contains(none, "-c shared_preload_libraries= ") && !strings.HasSuffix(none, "-c shared_preload_libraries=") {
		t.Fatalf("an empty list the user chose must win: %s", none)
	}
}

func TestOnlyOperatorsSeeStatementText(t *testing.T) {
	ws := newWorkspace(t)
	ws.person(t, "vic", "viewer")
	viewer := ws.signIn(t, "vic")
	for _, c := range [][2]string{{"GET", "/api/instances/orders/insights"}, {"POST", "/api/instances/orders/insights/reset"}, {"POST", "/api/instances/orders/sessions/1/cancel"}} {
		if code, _ := viewer.do(c[0], c[1], ""); code != 403 {
			t.Errorf("%s %s by a viewer: %d", c[0], c[1], code)
		}
	}
}
