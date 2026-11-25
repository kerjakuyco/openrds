package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestChangesOutliveTheClientButReadsDoNot(t *testing.T) {
	client, disconnect := context.WithCancel(withIdentity(t.Context(), identity{Username: "ana", Role: "operator"}))
	change, cancelChange := requestContext(client, http.MethodPost)
	defer cancelChange()
	read, cancelRead := requestContext(client, http.MethodGet)
	defer cancelRead()
	disconnect() // a closed browser tab or a dropped SSH tunnel
	if read.Err() == nil {
		t.Fatal("a read must end with its client")
	}
	if change.Err() != nil {
		t.Fatal("a snapshot or restore must keep going after its client leaves")
	}
	if deadline, ok := change.Deadline(); !ok || time.Until(deadline) < 23*time.Hour {
		t.Fatalf("a change must get hours, not the read limit: %v", deadline)
	}
	if id, ok := identityFrom(change); !ok || id.Username != "ana" {
		t.Fatal("a detached change must still be recorded under its user")
	}
}

func TestPostgresGetsSharedMemoryForParallelQueries(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	if err := m.provision(t.Context(), instanceSpec{Name: "capped", Engine: "postgres", Port: 15432, MemoryMiB: 2048}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.lastUp("OPENRDS_DB_SHM"); got != "2147483648" {
		t.Fatalf("/dev/shm must match the memory limit: %s", got)
	}
	if err := m.provision(t.Context(), instanceSpec{Name: "uncapped", Engine: "postgres", Port: 15433}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.lastUp("OPENRDS_DB_SHM"); got != "1073741824" {
		t.Fatalf("/dev/shm without a memory limit: %s", got)
	}
	for _, name := range []string{"postgres", "mysql", "valkey"} {
		data, err := os.ReadFile(filepath.Join("engines", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "stop_grace_period: 2m") || stopWait != "120" {
			t.Errorf("%s: Docker must give the database as long to stop as OpenRDS does", name)
		}
		if !strings.Contains(text, "start_period: 10m") || startWait != "900" {
			t.Errorf("%s: the start period must allow crash recovery and fit inside the start wait", name)
		}
	}
}

func TestBackupsStopBeforeTheDiskFills(t *testing.T) {
	if got := backupReserve(20 << 30); got != 2<<30 {
		t.Errorf("small disks keep 2 GiB: %d", got)
	}
	if got := backupReserve(100 << 30); got != 5<<30 {
		t.Errorf("disks keep 5%%: %d", got)
	}
	if got := backupReserve(4 << 40); got != 10<<30 {
		t.Errorf("large disks keep at most 10 GiB: %d", got)
	}

	var mu sync.Mutex
	free := uint64(50 << 30)
	setFree := func(n uint64) { mu.Lock(); free = n; mu.Unlock() }
	f := newFakeDocker()
	m := testManager(t, f.run)
	m.space = func(string) (uint64, uint64, bool) { mu.Lock(); defer mu.Unlock(); return free, 100 << 30, true } // keeps 5 GiB
	if err := m.provision(t.Context(), instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.makeBackup(t.Context(), "orders", "manual"); err != nil {
		t.Fatal(err)
	}
	setFree(5<<30 - 1)
	if _, err := m.makeBackup(t.Context(), "orders", "manual"); status(err) != 507 || !strings.Contains(err.Error(), "keeps 5.0 GiB free") {
		t.Fatalf("a backup must not start below the reserve: %v", err)
	}
	setFree(10 << 30)
	if err := m.checkBackupSpace(4 << 30); err != nil {
		t.Fatal(err)
	}
	if err := m.checkBackupSpace(6 << 30); status(err) != 507 || !strings.Contains(err.Error(), "needs about 6.0 GiB") {
		t.Fatalf("the expected size must fit above the reserve: %v", err)
	}

	// Space that runs out while the dump is written stops it, and nothing is published.
	defer func(n int) { guardEvery = n }(guardEvery)
	guardEvery = 1 << 10
	m.docker = runFunc(func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
		if args[0] == "compose" && composeVerb(args) == "exec" && out != nil {
			for range 8 {
				if _, err := out.Write(make([]byte, 512)); err != nil {
					return fmt.Errorf("write /dev/stdout: broken pipe")
				}
				setFree(1 << 30) // another database grows meanwhile
			}
		}
		return nil
	})
	before, _ := m.backups()
	if _, err := m.makeBackup(t.Context(), "orders", "manual"); status(err) != 507 || !strings.Contains(err.Error(), "backup stopped") {
		t.Fatalf("a backup must stop when free space runs low: %v", err)
	}
	after, _ := m.backups()
	entries, _ := os.ReadDir(m.backupDir())
	if len(after) != len(before) || len(entries) != len(before) {
		t.Fatalf("a stopped backup must leave nothing behind: %d backups, %d entries", len(after), len(entries))
	}
}

func TestMySQLBackupsAreCompressed(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := (backup{Name: "orders", Engine: "mysql", Created: when, Compression: "gzip"}).fileName(); got != "orders-20260102-030405.sql.gz" {
		t.Errorf("compressed download name: %s", got)
	}
	if got := (backup{Name: "orders", Engine: "postgres", Created: when}).fileName(); got != "orders-20260102-030405.dump" {
		t.Errorf("download name: %s", got)
	}
	m := testManager(t, newFakeDocker().run)
	b := backup{ID: randomHex(16), Name: "orders", Engine: "mysql", Version: "8.4", Created: when, Bytes: 4, Kind: "manual", Compression: "zstd"}
	dir := filepath.Join(m.backupDir(), b.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), b); err != nil {
		t.Fatal(err)
	}
	err := m.provision(t.Context(), instanceSpec{Name: "restored", Port: 13306}, &b)
	if status(err) != 400 || !strings.Contains(err.Error(), "Update OpenRDS") {
		t.Fatalf("a backup compressed in an unknown way must be refused before anything starts: %v", err)
	}
	if _, err := m.record("restored"); status(err) != 404 {
		t.Fatal("a refused restore must not reserve the name")
	}
}

func TestMemorySettingsFollowTheMemoryLimit(t *testing.T) {
	cases := []struct {
		engine string
		memory int
		want   map[string]string
	}{
		{"postgres", 0, nil},
		{"postgres", 128, map[string]string{"shared_buffers": "32MB", "effective_cache_size": "96MB", "maintenance_work_mem": "64MB"}},
		{"postgres", 4096, map[string]string{"shared_buffers": "1GB", "effective_cache_size": "3GB", "maintenance_work_mem": "256MB"}},
		{"postgres", 65536, map[string]string{"shared_buffers": "16GB", "effective_cache_size": "48GB", "maintenance_work_mem": "2GB"}},
		{"mysql", 0, nil},
		// The pool is a whole number of 128M chunks per instance, so MySQL never rounds it up.
		{"mysql", 512, map[string]string{"innodb_buffer_pool_size": "128M", "innodb_buffer_pool_instances": "1"}},
		{"mysql", 1024, map[string]string{"innodb_buffer_pool_size": "512M", "innodb_buffer_pool_instances": "1"}},
		{"mysql", 2048, map[string]string{"innodb_buffer_pool_size": "1280M", "innodb_buffer_pool_instances": "1"}},
		{"mysql", 4096, map[string]string{"innodb_buffer_pool_size": "2816M", "innodb_buffer_pool_instances": "2"}},
		{"mysql", 16384, map[string]string{"innodb_buffer_pool_size": "11264M", "innodb_buffer_pool_instances": "8"}},
		{"valkey", 1024, nil}, // maxmemory already follows the limit
	}
	for _, c := range cases {
		got := tunedParameters(c.engine, c.memory)
		if !maps.Equal(got, c.want) {
			t.Errorf("%s with %d MiB: got %v, want %v", c.engine, c.memory, got, c.want)
		}
		// Every sized value a user can also set must pass the same checks.
		settable := maps.Clone(got)
		delete(settable, "innodb_buffer_pool_instances")
		if err := checkParameters(c.engine, c.memory, settable); err != nil {
			t.Errorf("%s with %d MiB: %v", c.engine, c.memory, err)
		}
	}
	r := record{instanceSpec: instanceSpec{Engine: "postgres", MemoryMiB: 4096, Parameters: map[string]string{"shared_buffers": "512MB"}}}
	cmd := strings.Join(r.command(), " ")
	for _, want := range []string{"-c effective_cache_size=3GB", "-c maintenance_work_mem=256MB", "-c shared_buffers=512MB"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q lacks %q", cmd, want)
		}
	}
	if strings.Contains(cmd, "shared_buffers=1GB") {
		t.Error("a parameter the user set must win over the sized one")
	}
	if len(r.Parameters) != 1 {
		t.Error("sizing must not change the stored parameters")
	}
	own := record{instanceSpec: instanceSpec{Engine: "mysql", MemoryMiB: 4096, Parameters: map[string]string{"innodb_buffer_pool_size": "1G"}}}
	if p := own.effectiveParameters(); p["innodb_buffer_pool_size"] != "1G" || p["innodb_buffer_pool_instances"] != "" {
		t.Errorf("a pool the user sizes keeps MySQL's own instance count: %v", p)
	}
}

func TestNewTunablesAreChecked(t *testing.T) {
	ok := map[string]map[string]string{
		"postgres": {"checkpoint_completion_target": "0.7", "wal_compression": "lz4", "synchronous_commit": "local", "autovacuum_max_workers": "16",
			"autovacuum_naptime": "30", "autovacuum_vacuum_scale_factor": "0.05", "autovacuum_analyze_scale_factor": "0.02", "autovacuum_vacuum_cost_limit": "2000",
			"max_parallel_workers": "4", "max_parallel_maintenance_workers": "4", "idle_session_timeout": "600000", "track_io_timing": "on", "hash_mem_multiplier": "3"},
		"mysql": {"innodb_io_capacity_max": "40000", "innodb_log_buffer_size": "128M", "sort_buffer_size": "1M", "join_buffer_size": "1M",
			"max_execution_time": "30000", "innodb_parallel_read_threads": "8"},
		"valkey": {"activedefrag": "yes", "maxmemory-eviction-tenacity": "50"},
	}
	for engine, params := range ok {
		if err := checkParameters(engine, 0, params); err != nil {
			t.Errorf("%s: %v", engine, err)
		}
	}
	refused := []struct {
		engine string
		params map[string]string
	}{
		{"postgres", map[string]string{"autovacuum_max_workers": "17"}}, // more than PostgreSQL 18+ has slots for
		{"postgres", map[string]string{"checkpoint_completion_target": "1.5"}},
		{"postgres", map[string]string{"synchronous_commit": "remote_apply"}}, // no replicas
		{"mysql", map[string]string{"sort_buffer_size": "16K"}},
		{"valkey", map[string]string{"activedefrag": "on"}},
	}
	for _, c := range refused {
		if err := checkParameters(c.engine, 0, c.params); status(err) != 400 {
			t.Errorf("%s accepted %v", c.engine, c.params)
		}
	}
}

func TestBackupsRepeatEveryInterval(t *testing.T) {
	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	for _, c := range []struct {
		window   string
		interval int
		now      time.Time
		opened   time.Time
		open     bool
	}{
		{"03:00-03:30", 6, at(9, 10), at(9, 0), true},
		{"03:00-03:30", 6, at(9, 40), at(9, 0), false},
		{"03:00-03:30", 6, at(2, 0), at(-3, 0), false},    // yesterday's 21:00 opening
		{"23:45-00:15", 12, at(11, 50), at(11, 45), true}, // repeats from a window past midnight
		{"23:45-00:15", 12, at(0, 5), at(0, -15), true},   // yesterday's 23:45 opening
		{"03:00-06:00", 1, at(4, 59), at(4, 0), true},     // a long window lasts at most the interval
		{"03:00-03:30", 24, at(9, 10), at(3, 0), false},
		{"03:00-03:30", 0, at(3, 10), at(3, 0), true}, // 0 is daily
	} {
		opened, open := backupWindow(c.window, c.interval, c.now)
		if !opened.Equal(c.opened) || open != c.open {
			t.Errorf("%s every %dh at %s: opened %s open %v", c.window, c.interval, c.now.Format("15:04"), opened.Format("Jan 2 15:04"), open)
		}
	}
	if next := nextBackup("03:00-03:30", 4, at(3, 1), at(3, 2)); !next.Equal(at(7, 0)) {
		t.Fatalf("the next of every 4 hours: %v", next)
	}
	if next := nextBackup("03:00-03:30", 4, at(3, 1), at(23, 50)); !next.Equal(at(27, 0)) {
		t.Fatalf("the last opening of the day leads to tomorrow's first: %v", next)
	}

	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	now := time.Now().UTC()
	window := now.Add(-5*time.Minute).Format("15:04") + "-" + now.Add(25*time.Minute).Format("15:04")
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, BackupRetentionDays: 1, BackupWindow: window, BackupIntervalHours: 6}, nil); err != nil {
		t.Fatal(err)
	}
	if due := m.dueBackups(ctx, now); !slices.Equal(due, []string{"orders"}) {
		t.Fatalf("due in the first opening: %v", due)
	}
	m.automatedBackup(ctx, "orders")
	if due := m.dueBackups(ctx, now.Add(time.Minute)); len(due) != 0 {
		t.Fatal("one opening started two backups")
	}
	if due := m.dueBackups(ctx, now.Add(3*time.Hour)); len(due) != 0 {
		t.Fatal("a backup between openings")
	}
	if due := m.dueBackups(ctx, now.Add(6*time.Hour)); !slices.Equal(due, []string{"orders"}) {
		t.Fatalf("due 6 hours later: %v", due)
	}
	items, _ := m.instances(ctx)
	if items[0].BackupIntervalHours != 6 || items[0].NextBackup == nil {
		t.Fatalf("the view: %+v", items[0])
	}

	if _, err := m.modify(ctx, "orders", modification{BackupIntervalHours: ptr(5)}); status(err) != 400 {
		t.Fatal("5 hours does not divide a day and must be refused")
	}
	if _, err := m.modify(ctx, "orders", modification{BackupIntervalHours: ptr(24)}); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.record("orders"); r.BackupIntervalHours != 0 {
		t.Fatalf("daily is stored as before intervals existed: %d", r.BackupIntervalHours)
	}
	if items, _ := m.instances(ctx); items[0].BackupIntervalHours != 24 {
		t.Fatalf("the view reports daily as 24 hours: %d", items[0].BackupIntervalHours)
	}
}
