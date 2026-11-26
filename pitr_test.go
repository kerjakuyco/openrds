package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPointInTimeRecoveryIsChecked(t *testing.T) {
	for _, s := range []instanceSpec{
		{Name: "cache", Engine: "valkey", Port: 16379, BackupRetentionDays: 7, PITR: true},
		{Name: "orders", Engine: "postgres", Port: 15432, PITR: true}, // no automated backups
	} {
		if err := checkSpec(s); status(err) != 400 {
			t.Errorf("accepted %+v: %v", s, err)
		}
	}
	if err := checkSpec(instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, BackupRetentionDays: 7, PITR: true}); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(record{instanceSpec: instanceSpec{Engine: "postgres", PITR: true}}.command(), " ")
	for _, want := range []string{"archive_mode=on", "archive_timeout=60", "archive_command=test ! -f /archive/%f"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q: %s", want, cmd)
		}
	}
	if !strings.Contains(pgHBA(record{}), "local replication all trust") {
		t.Fatal("base backups need local replication connections")
	}
	pending := record{instanceSpec: instanceSpec{PITR: true}, Applied: &runtimeConfig{}}
	if pending.archiving() || !(record{instanceSpec: instanceSpec{PITR: true}}).archiving() {
		t.Fatal("archiving follows the running container until a restart applies the setting")
	}
}

func TestWALNamesCarryTheirArchiveTime(t *testing.T) {
	for _, name := range []string{"000000010000000000000003", "00000002.history", "000000010000000000000003.00000028.backup", "000000010000000000000004.partial"} {
		f, ok := parseWAL(fmt.Sprintf("1795672695_%s.gz", name))
		if !ok || f.Name != name || f.At.Unix() != 1795672695 {
			t.Errorf("%s: %+v %v", name, f, ok)
		}
	}
	for _, bad := range []string{"1795672695_../etc.gz", "x_000000010000000000000003.gz", "1795672695_000000010000000000000003", ".uploaded"} {
		if _, ok := parseWAL(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func writeWALFile(t *testing.T, m *manager, key string, at time.Time, name string) {
	t.Helper()
	dir := m.walDir(key)
	os.MkdirAll(dir, 0700)
	if err := storeWAL(dir, at, name, strings.NewReader("wal "+name)); err != nil {
		t.Fatal(err)
	}
}

func physicalBackup(t *testing.T, m *manager, name, key string, started, created time.Time) backup {
	t.Helper()
	b := backup{ID: randomHex(16), Name: name, Engine: "postgres", Version: "17", Kind: "automated", Format: "physical", DataKey: key,
		Started: started, Created: created, Bytes: 4, Compression: "gzip"}
	dir := filepath.Join(m.backupDir(), b.ID)
	os.MkdirAll(dir, 0700)
	var data bytes.Buffer
	z := gzip.NewWriter(&data)
	tw := tar.NewWriter(z)
	tw.WriteHeader(&tar.Header{Name: "PG_VERSION", Mode: 0600, Size: 3})
	tw.Write([]byte("17\n"))
	tw.Close()
	z.Close()
	os.WriteFile(filepath.Join(dir, "data"), data.Bytes(), 0600)
	if err := writeJSON(filepath.Join(dir, "meta.json"), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheRecoveryWindowAndItsBaseBackups(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, BackupRetentionDays: 7, PITR: true}, Project: "openrds-db-orders"}
	if err := writeJSON(m.recordPath(r.Name), r); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	if _, _, err := m.baseFor(r, day); status(err) != 409 {
		t.Fatal("without a base backup there is no window")
	}
	first := physicalBackup(t, m, "orders", r.diskKey(), day.Add(time.Hour), day.Add(time.Hour+time.Minute))
	second := physicalBackup(t, m, "orders", r.diskKey(), day.Add(25*time.Hour), day.Add(25*time.Hour+time.Minute))
	physicalBackup(t, m, "orders", "openrds-db-orders-1", day.Add(30*time.Hour), day.Add(30*time.Hour)) // after an upgrade: other data
	for h := 0; h <= 28; h++ {
		writeWALFile(t, m, r.diskKey(), day.Add(time.Duration(h)*time.Hour), fmt.Sprintf("0000000100000000000000%02X", h))
	}
	backups, _ := m.backups()
	w := m.window(r, backups)
	if !w.Earliest.Equal(first.Created) || !w.Latest.Equal(day.Add(28*time.Hour)) {
		t.Fatalf("window: %v to %v", w.Earliest, w.Latest)
	}
	if b, _, err := m.baseFor(r, day.Add(10*time.Hour)); err != nil || b.ID != first.ID {
		t.Fatalf("a time on the first day replays from the first base backup: %+v %v", b, err)
	}
	if b, _, err := m.baseFor(r, day.Add(26*time.Hour)); err != nil || b.ID != second.ID {
		t.Fatalf("a later time replays from the newest base backup before it: %+v %v", b, err)
	}
	for _, at := range []time.Time{day, day.Add(29 * time.Hour)} {
		if _, _, err := m.baseFor(r, at); status(err) != 400 || !strings.Contains(err.Error(), "Choose a time from") {
			t.Errorf("%v is outside the window: %v", at, err)
		}
	}
	files, open, err := m.walForRestore(t.Context(), second, day.Add(26*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "000000010000000000000019" || files[1].Name != "00000001000000000000001A" {
		t.Fatalf("WAL from the base backup's start to a little past the target: %+v", files)
	}
	var stream bytes.Buffer
	if err := writeWALTar(&stream, files, open); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&stream)
	h, _ := tr.Next()
	body, _ := io.ReadAll(tr)
	if h.Name != "000000010000000000000019" || string(body) != "wal 000000010000000000000019" {
		t.Fatalf("the stream carries plain WAL under its own name: %s %q", h.Name, body)
	}

	// Pruning keeps what the oldest base backup needs.
	m.pruneWAL(t.Context(), backups)
	left, _ := m.walFiles(r.diskKey())
	if len(left) != 28 || left[0].At != day.Add(time.Hour) {
		t.Fatalf("WAL before the oldest base backup is removed: %d left, first %v", len(left), left[0].At)
	}
	os.RemoveAll(filepath.Join(m.backupDir(), first.ID))
	os.RemoveAll(filepath.Join(m.backupDir(), second.ID))
	backups, _ = m.backups()
	m.pruneWAL(t.Context(), backups)
	if left, _ := m.walFiles(r.diskKey()); len(left) != 0 {
		t.Fatalf("with no base backup left, its WAL goes: %d", len(left))
	}
}

func TestWALIsCollectedThenRemovedFromTheArchive(t *testing.T) {
	var mu sync.Mutex
	var removed string
	m := testManager(t, func(_ context.Context, _ []string, in io.Reader, out io.Writer, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		if args[0] != "compose" || composeVerb(args) != "exec" {
			return nil
		}
		script := args[len(args)-1]
		switch {
		case strings.Contains(script, "tar -cf"):
			tw := tar.NewWriter(out)
			for i, name := range []string{"000000010000000000000001", "000000010000000000000002", "../escape"} {
				tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 4, ModTime: time.Unix(1795672600+int64(i), 0)})
				tw.Write([]byte("data"))
			}
			return tw.Close()
		case strings.Contains(script, "xargs rm"):
			b, _ := io.ReadAll(in)
			removed = string(b)
		}
		return nil
	})
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres"}, Project: "openrds-db-orders"}
	n, err := m.collectWAL(t.Context(), r)
	if err != nil || n != 2 {
		t.Fatalf("collected %d: %v", n, err)
	}
	files, _ := m.walFiles(r.diskKey())
	if len(files) != 2 || files[1].At.Unix() != 1795672601 {
		t.Fatalf("stored: %+v", files)
	}
	if removed != "000000010000000000000001\n000000010000000000000002\n" {
		t.Fatalf("only stored files leave the archive, never a stray path: %q", removed)
	}
	if !m.latestWAL(r.diskKey()).Equal(time.Unix(1795672601, 0)) {
		t.Fatal("the window's end follows collection")
	}
}

func TestAPhysicalRestoreReplaysToTheTarget(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	src := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Version: "17", Port: 15432, BackupRetentionDays: 7, PITR: true, MemoryMiB: 512}, Project: "openrds-db-orders"}
	writeJSON(m.recordPath(src.Name), src)
	b := physicalBackup(t, m, "orders", src.diskKey(), day, day.Add(time.Minute))
	writeWALFile(t, m, src.diskKey(), day.Add(time.Minute), "000000010000000000000002")
	writeWALFile(t, m, src.diskKey(), day.Add(2*time.Hour), "000000010000000000000003")

	if err := m.provisionAt(ctx, instanceSpec{Name: "newer", Version: "18", Port: 15433}, &b, nil); status(err) != 400 {
		t.Fatal("a physical backup restores only into its own version")
	}
	if err := m.provisionAt(ctx, instanceSpec{Name: "logical", Port: 15433}, &backup{Engine: "postgres"}, &day); status(err) != 400 {
		t.Fatal("a logical backup cannot be rolled forward")
	}
	err := m.restoreToTime(ctx, "orders", restoreToTimeRequest{Name: "orders-at", Port: 15433, TargetTime: day.Add(90 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := m.record("orders-at")
	if r.PendingRestore || r.MemoryMiB != 512 || r.PITR || r.Version != "17" {
		t.Fatalf("the new database has the source's size, no archiving of its own yet: %+v", r)
	}
	f.mu.Lock()
	inputs := strings.Join(f.inputs, "\n")
	var scripts []string
	for _, a := range f.args {
		scripts = append(scripts, a[len(a)-1])
	}
	f.mu.Unlock()
	all := strings.Join(scripts, "\n")
	for _, want := range []string{`tar -x -C "$PGDATA"`, "recovery_target_time = '2026-03-09 01:30:00+00'", "recovery.signal", `chown -R postgres:postgres "$PGDATA"`, `rm -rf "$PGDATA/openrds_wal"`} {
		if !strings.Contains(all, want) {
			t.Errorf("the restore lacks %q", want)
		}
	}
	if !strings.Contains(inputs, "ALTER ROLE openrds PASSWORD 'SCRAM-SHA-256$") || !strings.Contains(inputs, "NOLOGIN PASSWORD NULL") {
		t.Fatal("the restored database gets its own owner password, and the source's users are turned off")
	}
	items, _ := m.readActivity()
	if items[0].Action != "Restore to point in time" || items[0].Result != "completed" || !strings.Contains(items[0].Detail, "orders as of 2026-03-09 01:30:00 UTC") {
		t.Fatalf("events: %+v", items[0])
	}
}
