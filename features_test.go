package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A small stand-in for Docker that remembers which Compose projects run.
type fakeDocker struct {
	mu       sync.Mutex
	running  map[string]bool
	ups      [][]string // environment of each compose up
	args     [][]string // every compose invocation
	failUp   func(env []string) bool
	failExec bool
	inputs   []string // stdin of each compose exec
}

func newFakeDocker() *fakeDocker { return &fakeDocker{running: make(map[string]bool)} }

func composeVerb(args []string) string {
	i := 3
	for i < len(args) && args[i] == "--file" {
		i += 2
	}
	return args[i]
}

func (f *fakeDocker) run(_ context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "ps":
		for project, on := range f.running {
			if on {
				fmt.Fprintf(out, "%s|running|Up 1 minute (healthy)\n", project)
			} else {
				fmt.Fprintf(out, "%s|exited|Exited (0) 1 second ago\n", project)
			}
		}
	case "compose":
		f.args = append(f.args, args)
		project := args[2]
		switch composeVerb(args) {
		case "up":
			f.ups = append(f.ups, env)
			if f.failUp != nil && f.failUp(env) {
				f.running[project] = false
				return errors.New("Bind for 127.0.0.1:15432 failed: port is already allocated")
			}
			f.running[project] = true
		case "stop":
			if slices.Contains(args, "db") { // stopping only the gateway or pooler leaves the database running
				f.running[project] = false
			}
		case "down":
			delete(f.running, project)
		case "exec":
			if f.failExec || !f.running[project] {
				return errors.New("service \"db\" is not running")
			}
			if in != nil { // a restore, or a dump that reads the password first
				data, err := io.ReadAll(in)
				if err != nil {
					return err
				}
				f.inputs = append(f.inputs, string(data))
			}
			if out != nil {
				_, err := out.Write([]byte("logical dump"))
				return err
			}
		}
	}
	return nil
}

func (f *fakeDocker) upCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ups)
}

func (f *fakeDocker) lastUp(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.ups[len(f.ups)-1] {
		if value, ok := strings.CutPrefix(v, key+"="); ok {
			return value
		}
	}
	return ""
}

func status(err error) int {
	var p *problem
	if errors.As(err, &p) {
		return p.status
	}
	return 0
}

func ptr[T any](v T) *T { return &v }

func TestParametersAreCheckedBeforeTheyReachACommandLine(t *testing.T) {
	refused := []struct {
		engine string
		memory int
		params map[string]string
	}{
		{"postgres", 0, map[string]string{"fsync": "off"}},
		{"postgres", 0, map[string]string{"max_connections": "1"}},
		{"postgres", 0, map[string]string{"max_connections": "100 -c fsync=off"}},
		{"postgres", 0, map[string]string{"work_mem": "4 MB"}},
		{"postgres", 0, map[string]string{"work_mem": "4M"}},
		{"postgres", 0, map[string]string{"min_wal_size": "16MB"}},
		{"postgres", 512, map[string]string{"shared_buffers": "512MB"}},
		{"postgres", 0, map[string]string{"shared_preload_libraries": "pg_stat_statements,pg_stat_statements"}},
		{"postgres", 0, map[string]string{"random_page_cost": "1e3"}},
		{"mysql", 0, map[string]string{"innodb_buffer_pool_size": "128MB"}},
		{"mysql", 0, map[string]string{"sql_mode": "ANSI_QUOTES,--skip-grant-tables"}},
		{"valkey", 0, map[string]string{"save": "60"}},
		{"valkey", 0, map[string]string{"notify-keyspace-events": "Ex --loadmodule"}},
		{"valkey", 0, map[string]string{"appendonly": "yes"}},
	}
	for _, c := range refused {
		if err := checkParameters(c.engine, c.memory, c.params); status(err) != 400 {
			t.Errorf("%s accepted %v: %v", c.engine, c.params, err)
		}
	}
	pg := map[string]string{"shared_buffers": "256MB", "max_connections": "200", "random_page_cost": "1.1",
		"shared_preload_libraries": "pg_stat_statements,auto_explain"}
	if err := checkParameters("postgres", 1024, pg); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		spec instanceSpec
		want []string
	}{
		{instanceSpec{Engine: "postgres", Parameters: pg}, []string{"postgres", "-c", "ssl=on", "-c", "ssl_cert_file=/run/openrds/server.crt",
			"-c", "ssl_key_file=/run/openrds/server.key", "-c", "hba_file=/run/openrds/pg_hba.conf", "-c", "max_connections=200", "-c", "random_page_cost=1.1",
			"-c", "shared_buffers=256MB", "-c", "shared_preload_libraries=pg_stat_statements,auto_explain"}},
		{instanceSpec{Engine: "mysql", RequireTLS: true, Parameters: map[string]string{"max_connections": "300", "sql_mode": ""}},
			[]string{"mysqld", "--ssl-ca=/run/openrds/ca.crt", "--ssl-cert=/run/openrds/server.crt", "--ssl-key=/run/openrds/server.key",
				"--require-secure-transport=ON", "--max_connections=300", "--sql_mode="}},
		{instanceSpec{Engine: "valkey", Parameters: map[string]string{"maxmemory-policy": "allkeys-lru", "notify-keyspace-events": "Ex$", "save": ""}},
			[]string{"valkey-server", "--include", "/run/openrds/users.conf", "--dir", "/data",
				"--maxmemory", "${OPENRDS_VALKEY_MAXMEMORY:-0}", "--maxmemory-policy", "allkeys-lru", "--notify-keyspace-events", "Ex$$", "--save", ""}},
	}
	for _, c := range cases {
		if err := checkParameters(c.spec.Engine, 0, c.spec.Parameters); err != nil {
			t.Fatal(err)
		}
		if got := (record{instanceSpec: c.spec}).command(); !slices.Equal(got, c.want) {
			t.Errorf("%s command:\n got %q\nwant %q", c.spec.Engine, got, c.want)
		}
	}
}

func TestComposeOverrideCarriesCommandAndSecretsDigest(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := m.record("orders")
	if r.Layout != currentLayout {
		t.Fatalf("a started container must record its layout: %d", r.Layout)
	}
	var doc struct {
		Services struct {
			DB struct {
				Command []string          `json:"command"`
				Labels  map[string]string `json:"labels"`
			} `json:"db"`
		} `json:"services"`
	}
	read := func() {
		t.Helper()
		if !slices.Contains(f.args[len(f.args)-1], m.overridePath(r)) {
			t.Fatalf("the override was not passed to Compose: %v", f.args[len(f.args)-1])
		}
		if err := readJSON(m.overridePath(r), &doc); err != nil {
			t.Fatal(err)
		}
	}
	read()
	before := doc.Services.DB.Labels["io.openrds.secrets"]
	if before == "" || f.lastUp("OPENRDS_TLS_CERT") == "" || !strings.Contains(f.lastUp("OPENRDS_PG_HBA"), "host all all all scram-sha-256") {
		t.Fatal("TLS files and pg_hba.conf must reach Compose")
	}
	if _, err := m.modify(ctx, "orders", modification{Parameters: map[string]string{"max_connections": "250"}, RequireTLS: ptr(true), ApplyImmediately: true}); err != nil {
		t.Fatal(err)
	}
	read()
	if !slices.Contains(doc.Services.DB.Command, "max_connections=250") || doc.Services.DB.Labels["io.openrds.secrets"] == before {
		t.Fatalf("override: %+v", doc)
	}
	if strings.Contains(f.lastUp("OPENRDS_PG_HBA"), "host all all all scram-sha-256") {
		t.Fatal("requiring TLS must drop plain-text remote connections")
	}
	if err := m.remove(ctx, "orders", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.overridePath(r)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleting the instance left its override behind")
	}
	if _, err := os.Stat(m.tlsPath(r.Project + ".key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleting the instance left its TLS key behind")
	}
}

func TestModifyDefersOrAppliesRuntimeChanges(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	ups := f.upCount()
	res, err := m.modify(ctx, "orders", modification{CPUs: ptr(2.0)})
	r, _ := m.record("orders")
	if err != nil || !res.PendingRestart || res.Restarted || f.upCount() != ups || r.CPUs != 2 || r.Applied == nil || r.Applied.CPUs != 0 {
		t.Fatalf("deferred change: %+v %+v %v", res, r, err)
	}
	if res, err = m.modify(ctx, "orders", modification{CPUs: ptr(0.0)}); err != nil || res.PendingRestart {
		t.Fatalf("a reverted change is still pending: %+v %v", res, err)
	}
	if r, _ = m.record("orders"); r.Applied != nil {
		t.Fatal("reverted change kept its pending state")
	}
	if _, err = m.modify(ctx, "orders", modification{MemoryMiB: ptr(256)}); err != nil {
		t.Fatal(err)
	}
	if err := m.change(ctx, "orders", "reboot"); err != nil {
		t.Fatal(err)
	}
	if r, _ = m.record("orders"); r.Applied != nil || f.lastUp("OPENRDS_DB_MEMORY") != "268435456" {
		t.Fatal("reboot did not apply the pending memory limit")
	}
	if res, err = m.modify(ctx, "orders", modification{MemoryMiB: ptr(512), ApplyImmediately: true}); err != nil || !res.Restarted || f.lastUp("OPENRDS_DB_MEMORY") != "536870912" {
		t.Fatalf("immediate change: %+v %v", res, err)
	}
	if res, err = m.modify(ctx, "orders", modification{Port: ptr(15433)}); err != nil || !res.Restarted || f.lastUp("OPENRDS_DB_PORT") != "15433" {
		t.Fatalf("a port change must restart a running database: %+v %v", res, err)
	}
	if err := m.provision(ctx, instanceSpec{Name: "billing", Engine: "mysql", Port: 13306}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = m.modify(ctx, "orders", modification{Port: ptr(13306)}); status(err) != 409 {
		t.Fatalf("port conflict: %v", err)
	}
	if _, err = m.modify(ctx, "orders", modification{MemoryMiB: ptr(64)}); status(err) != 400 {
		t.Fatalf("invalid memory limit: %v", err)
	}
	ups = f.upCount()
	tags := map[string]string{"env": "production", "team": "payments"}
	if res, err = m.modify(ctx, "orders", modification{DeletionProtection: ptr(true), Tags: tags, BackupRetentionDays: ptr(7)}); err != nil || res.Restarted || res.PendingRestart || f.upCount() != ups {
		t.Fatalf("settings that need no restart: %+v %v", res, err)
	}
	if err := m.remove(ctx, "orders", false); status(err) != 409 {
		t.Fatalf("deletion protection did not block delete: %v", err)
	}
	if err := m.change(ctx, "orders", "stop"); err != nil {
		t.Fatal(err)
	}
	if res, err = m.modify(ctx, "orders", modification{CPUs: ptr(1.5)}); err != nil || res.PendingRestart || res.Restarted {
		t.Fatalf("a stopped database applies changes at its next start: %+v %v", res, err)
	}
	if err := m.change(ctx, "orders", "reboot"); status(err) != 409 {
		t.Fatal("rebooted a stopped database")
	}
	if _, err = m.modify(ctx, "orders", modification{DeletionProtection: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := m.remove(ctx, "orders", false); err != nil {
		t.Fatal(err)
	}
}

func TestFailedImmediateChangeRestoresThePreviousSettings(t *testing.T) {
	f := newFakeDocker()
	f.failUp = func(env []string) bool { return slices.Contains(env, "OPENRDS_DB_PORT=15500") }
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, MemoryMiB: 256}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := m.modify(ctx, "orders", modification{Port: ptr(15500), MemoryMiB: ptr(1024)})
	if status(err) != 502 || !strings.Contains(err.Error(), "previous settings") {
		t.Fatalf("expected a rollback, got %v", err)
	}
	r, _ := m.record("orders")
	if r.Port != 15432 || r.MemoryMiB != 256 || f.lastUp("OPENRDS_DB_PORT") != "15432" || !f.running[r.Project] {
		t.Fatalf("previous settings were not restored: %+v", r)
	}
}

func TestFinalSnapshotIsTakenBeforeDeletion(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "valkey", Port: 16379, Parameters: map[string]string{"maxmemory-policy": "allkeys-lfu"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.remove(ctx, "orders", true); err != nil {
		t.Fatal(err)
	}
	list, _ := m.backups()
	if len(list) != 1 || !list[0].Final || list[0].Kind != "manual" || list[0].Expires != nil || list[0].Parameters["maxmemory-policy"] != "allkeys-lfu" {
		t.Fatalf("final snapshot: %+v", list)
	}
	if _, err := m.record("orders"); status(err) != 404 {
		t.Fatal("instance was not deleted")
	}
	if err := m.provision(ctx, instanceSpec{Name: "billing", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	f.failExec = true
	if err := m.remove(ctx, "billing", true); status(err) != 502 {
		t.Fatalf("failed final snapshot: %v", err)
	}
	if _, err := m.record("billing"); err != nil {
		t.Fatal("instance deleted although its final snapshot failed")
	}
	if list, _ := m.backups(); len(list) != 1 {
		t.Fatal("partial final snapshot published")
	}
}

func TestBackupWindows(t *testing.T) {
	for _, c := range []struct {
		window        string
		start, length int
		ok            bool
	}{
		{"03:00-03:30", 180, 30, true},
		{"23:45-00:15", 1425, 30, true},
		{"22:00-02:00", 1320, 240, true},
		{"03:00-03:10", 180, 10, false},
		{"03:00-03:00", 180, 0, false},
		{"24:00-01:00", 0, 0, false},
		{"3:00-4:00", 0, 0, false},
	} {
		start, length, ok := parseWindow(c.window)
		if start != c.start || length != c.length || ok != c.ok {
			t.Errorf("%s: %d %d %v", c.window, start, length, ok)
		}
	}
	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	opened, open := backupWindow("23:45-00:15", 0, day.Add(5*time.Minute))
	if !open || !opened.Equal(day.Add(-15*time.Minute)) {
		t.Fatalf("a window past midnight: %v %v", opened, open)
	}
	if _, open := backupWindow("03:00-03:30", 0, day.Add(3*time.Hour+30*time.Minute)); open {
		t.Fatal("the window end is exclusive")
	}
	if next := nextBackup("03:00-03:30", 0, time.Time{}, day.Add(time.Hour)); !next.Equal(day.Add(3 * time.Hour)) {
		t.Fatalf("next backup before the window: %v", next)
	}
	if next := nextBackup("03:00-03:30", 0, day.Add(3*time.Hour+time.Minute), day.Add(3*time.Hour+2*time.Minute)); !next.Equal(day.Add(27 * time.Hour)) {
		t.Fatalf("next backup after today's: %v", next)
	}
}

func TestAutomatedBackupsFollowTheWindowAndRetention(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	now := time.Now().UTC()
	window := now.Add(-5*time.Minute).Format("15:04") + "-" + now.Add(25*time.Minute).Format("15:04")
	spec := instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, BackupRetentionDays: 2, BackupWindow: window}
	if err := m.provision(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.provision(ctx, instanceSpec{Name: "scratch", Engine: "valkey", Port: 16379}, nil); err != nil {
		t.Fatal(err)
	}
	if due := m.dueBackups(ctx, now.Add(-10*time.Minute)); len(due) != 0 {
		t.Fatalf("due before the window: %v", due)
	}
	m.busy["orders"] = true
	if due := m.dueBackups(ctx, now); len(due) != 0 {
		t.Fatal("claimed a busy instance")
	}
	delete(m.busy, "orders")
	due := m.dueBackups(ctx, now)
	if !slices.Equal(due, []string{"orders"}) {
		t.Fatalf("due in the window: %v (instances without retention must be skipped)", due)
	}
	if again := m.dueBackups(ctx, now.Add(time.Minute)); len(again) != 0 {
		t.Fatal("one window started two backups")
	}
	m.automatedBackup(ctx, "orders")
	list, _ := m.backups()
	if len(list) != 1 || list[0].Kind != "automated" || list[0].Expires == nil || !list[0].Expires.Equal(list[0].Created.AddDate(0, 0, 2)) {
		t.Fatalf("automated backup: %+v", list)
	}
	auto := list[0]
	manual, err := m.makeBackup(ctx, "orders", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if due := m.dueBackups(ctx, now.Add(24*time.Hour)); !slices.Equal(due, []string{"orders"}) {
		t.Fatalf("the next day's window: %v", due)
	}
	if _, err := m.modify(ctx, "orders", modification{BackupRetentionDays: ptr(5)}); err != nil {
		t.Fatal(err)
	}
	if b, _ := m.backup(auto.ID); !b.Expires.Equal(b.Created.AddDate(0, 0, 5)) {
		t.Fatal("a longer retention period did not extend existing automated backups")
	}
	m.expireBackups(ctx, now.Add(4*24*time.Hour))
	if _, err := m.backup(auto.ID); err != nil {
		t.Fatal("expired before the new retention period")
	}
	m.expireBackups(ctx, now.Add(6*24*time.Hour))
	if _, err := m.backup(auto.ID); status(err) != 404 {
		t.Fatal("automated backup outlived its retention")
	}
	if _, err := m.backup(manual.ID); err != nil {
		t.Fatal("manual snapshot expired")
	}
	m.attempts = map[string]time.Time{}
	m.automatedBackup(ctx, "orders")
	list, _ = m.backups()
	kept, err := m.keepBackup(ctx, list[0].ID)
	if err != nil || kept.Kind != "manual" || kept.Expires != nil {
		t.Fatalf("keep: %+v %v", kept, err)
	}
	if _, err := m.keepBackup(ctx, kept.ID); status(err) != 409 {
		t.Fatal("kept a manual snapshot twice")
	}
	m.expireBackups(ctx, now.Add(30*24*time.Hour))
	if _, err := m.backup(kept.ID); err != nil {
		t.Fatal("a kept backup expired")
	}
	activity, _ := m.readActivity()
	var actions []string
	for _, e := range activity {
		actions = append(actions, e.Action+" "+e.Result)
	}
	for _, want := range []string{"Automated backup completed", "Expire automated backup completed", "Keep backup as snapshot completed"} {
		if !slices.Contains(actions, want) {
			t.Errorf("activity lacks %q: %v", want, actions)
		}
	}
}

func TestFailureDetailIsRecorded(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	f.failExec = true
	if _, err := m.makeBackup(ctx, "orders", "manual"); err == nil {
		t.Fatal("failed backup succeeded")
	}
	items, _ := m.readActivity()
	if items[0].Result != "failed" || !strings.Contains(items[0].Detail, "is not running") {
		t.Fatalf("failure detail: %+v", items[0])
	}
}

type fakeEngine map[string]rawStats

func (f fakeEngine) stats(_ context.Context, id string) (rawStats, error) {
	s, ok := f[id]
	if !ok {
		return s, errors.New("no such container")
	}
	return s, nil
}

func reading(at time.Time, cpu, system, rx, read uint64, usage uint64) rawStats {
	var s rawStats
	s.Read = at
	s.CPU.Usage.Total, s.CPU.System, s.CPU.Online = cpu, system, 4
	s.Memory.Usage, s.Memory.Limit = usage, 1<<30
	s.Memory.Stats = map[string]uint64{"inactive_file": 10 << 20}
	s.Networks = map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	}{"eth0": {Rx: rx, Tx: rx / 2}}
	s.Block.Bytes = []struct {
		Op    string `json:"op"`
		Value uint64 `json:"value"`
	}{{"read", read}, {"write", read * 2}}
	s.PIDs.Current = 7
	return s
}

func TestMetricHistoryRatesAndBuckets(t *testing.T) {
	var h history
	h.init(t.TempDir()+"/metrics.json", 30*time.Second)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	h.add("p", "aaaaaaaaaaaa", reading(t0, 1e9, 100e9, 0, 0, 110<<20), nil)
	if len(h.points["p"]) != 0 {
		t.Fatal("the first reading must only set a baseline")
	}
	// 30 s on a 4-core host is 120 s of host CPU time; 3 s of it is 10% of one core.
	h.add("p", "aaaaaaaaaaaa", reading(t0.Add(30*time.Second), 4e9, 220e9, 3000, 300, 110<<20), nil)
	p := h.points["p"][0]
	if p.CPU < 9.99 || p.CPU > 10.01 || p.Memory != 100<<20 || p.NetIn != 100 || p.NetOut != 50 || p.Read != 10 || p.Write != 20 || p.PIDs != 7 {
		t.Fatalf("rates: %+v", p)
	}
	h.add("p", "aaaaaaaaaaaa", reading(t0.Add(60*time.Second), 1e8, 340e9, 10, 10, 50<<20), nil)
	if p := h.points["p"][1]; p.CPU != 0 || p.NetIn != 0 || p.Read != 0 {
		t.Fatalf("a counter reset must read as zero: %+v", p)
	}
	h.add("p", "bbbbbbbbbbbb", reading(t0.Add(90*time.Second), 5e9, 460e9, 10, 10, 50<<20), nil)
	if len(h.points["p"]) != 2 {
		t.Fatal("a recreated container must start a new baseline")
	}
	for i := range 240 {
		h.add("q", "cccccccccccc", reading(t0.Add(time.Duration(i)*30*time.Second), uint64(i)*1e9, uint64(i)*100e9, uint64(i)*3000, 0, 100<<20), nil)
	}
	now := t0.Add(2 * time.Hour)
	if got := len(h.series("q", time.Hour, now)); got > 120 || got < 115 {
		t.Fatalf("1h at 30s resolution: %d points", got)
	}
	if body, _ := json.Marshal(h.view("new", time.Hour, now)); !strings.Contains(string(body), `"points":[]`) {
		t.Fatalf("a database without readings must send an empty list: %s", body)
	}
	day := h.series("q", 24*time.Hour, now)
	if len(day) < 19 || len(day) > 21 || day[1].NetIn != 100 {
		t.Fatalf("24h buckets: %d points, %+v", len(day), day[1])
	}
	last := t0.Add(239 * 30 * time.Second)
	if h.latest("q", last.Add(2*time.Minute)) != nil || h.latest("q", last.Add(30*time.Second)) == nil {
		t.Fatal("latest reading freshness")
	}
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	var reloaded history
	reloaded.init(h.path, h.interval)
	reloaded.load(now)
	if len(reloaded.points["q"]) != len(h.points["q"]) {
		t.Fatal("history did not survive a restart")
	}
	h.prune(map[string]bool{"q": true}, now)
	if _, ok := h.points["p"]; ok {
		t.Fatal("a deleted instance kept its history")
	}
}

func TestSamplingUsesOnlyKnownInstances(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	if err := m.provision(t.Context(), instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := m.record("orders")
	t0 := time.Now().Add(-time.Minute)
	engine := fakeEngine{"aaaaaaaaaaaa": reading(t0, 1e9, 100e9, 0, 0, 50<<20), "bbbbbbbbbbbb": reading(t0, 1e9, 100e9, 0, 0, 50<<20)}
	m.engine = engine
	m.docker = runFunc(func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
		if args[0] == "ps" {
			fmt.Fprintf(out, "aaaaaaaaaaaa|%s\nbbbbbbbbbbbb|openrds-db-someone-else\n", r.Project)
		}
		return nil
	})
	m.sample(t.Context(), time.Now())
	engine["aaaaaaaaaaaa"] = reading(t0.Add(30*time.Second), 2e9, 110e9, 600, 0, 50<<20)
	m.sample(t.Context(), time.Now())
	if len(m.history.points) != 1 || len(m.history.points[r.Project]) != 1 || m.history.points[r.Project][0].NetIn != 20 {
		t.Fatalf("history: %+v", m.history.points)
	}
}

func TestEngineCatalogAndHistoryEndpoints(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	if err := m.provision(t.Context(), instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, Tags: map[string]string{"env": "prod"}}, nil); err != nil {
		t.Fatal(err)
	}
	h := handler(m, testAuth(t, m, "test-token"))
	get := func(path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	code, body := get("/api/engines")
	var catalog []engineView
	if err := json.Unmarshal([]byte(body), &catalog); code != 200 || err != nil || len(catalog) != 3 || catalog[0].Key != "postgres" || len(catalog[1].Parameters) == 0 {
		t.Fatalf("engines: %d %v %s", code, err, body)
	}
	if code, _ := get("/api/instances/orders/history?range=2d"); code != 400 {
		t.Fatal("unknown chart range accepted")
	}
	m.history.unavailable = "Metric history is off."
	if code, body := get("/api/instances/orders/history?range=6h"); code != 200 || !strings.Contains(body, `"available":false`) {
		t.Fatalf("history: %d %s", code, body)
	}
	code, body = get("/api/instances")
	r, _ := m.record("orders")
	if code != 200 || !strings.Contains(body, `"tags":{"env":"prod"}`) || !strings.Contains(body, `"backupWindow":"03:00-03:30"`) || strings.Contains(body, r.Password) {
		t.Fatalf("inventory: %d %s", code, body)
	}
}
