package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- a binary log written the way MySQL writes it

func binlogEvent(kind byte, at time.Time, body []byte, pos *int64) []byte {
	size := 19 + len(body) + 4 // and a CRC32 checksum
	*pos += int64(size)
	h := make([]byte, 19)
	binary.LittleEndian.PutUint32(h[0:], uint32(at.Unix()))
	h[4] = kind
	binary.LittleEndian.PutUint32(h[5:], 1)
	binary.LittleEndian.PutUint32(h[9:], uint32(size))
	binary.LittleEndian.PutUint32(h[13:], uint32(*pos))
	return append(append(h, body...), 0, 0, 0, 0)
}

func gtidBody(committed time.Time) []byte {
	b := make([]byte, 1+16+8+1+16+7+1)
	b[0], b[25] = 1, 2 // logical timestamps follow
	us := uint64(committed.UnixMicro())
	for i := range 7 {
		b[42+i] = byte(us >> (8 * i))
	}
	return b
}

// A binary log with a transaction per commit time, closed by a rotate event,
// and the offset each transaction starts at.
func fakeBinlog(opened time.Time, commits ...time.Time) ([]byte, []int64, int64) {
	data := []byte{0xfe, 'b', 'i', 'n'}
	pos := int64(4)
	data = append(data, binlogEvent(15, opened, make([]byte, 100), &pos)...) // format description
	data = append(data, binlogEvent(35, opened, make([]byte, 8), &pos)...)   // previous GTIDs
	var starts []int64
	for _, c := range commits {
		starts = append(starts, pos)
		data = append(data, binlogEvent(anonymousGTIDEvent, c.Truncate(time.Second), gtidBody(c), &pos)...)
		data = append(data, binlogEvent(2, c, []byte("BEGIN"), &pos)...)
		data = append(data, binlogEvent(30, c, make([]byte, 40), &pos)...) // rows
		data = append(data, binlogEvent(16, c, make([]byte, 8), &pos)...)  // commit
	}
	end := pos
	data = append(data, binlogEvent(rotateEvent, opened, []byte("binlog.next"), &pos)...)
	return data, starts, end
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write(data)
	z.Close()
	return b.Bytes()
}

func TestMySQLPointInTimeRecoveryIsChecked(t *testing.T) {
	ok := instanceSpec{Name: "shop", Engine: "mysql", Port: 13306, BackupRetentionDays: 7, PITR: true}
	if err := checkSpec(ok); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"0", "86400", "2592000"} {
		s := ok
		s.Parameters = map[string]string{"binlog_expire_logs_seconds": keep}
		if err := checkSpec(s); err != nil {
			t.Errorf("%s seconds: %v", keep, err)
		}
	}
	short := ok
	short.Parameters = map[string]string{"binlog_expire_logs_seconds": "3600"}
	if err := checkSpec(short); status(err) != 400 || !strings.Contains(err.Error(), "at least a day") {
		t.Fatalf("binary logs kept an hour could go before they are copied: %v", err)
	}
	off := ok
	off.PITR = false
	if !ok.runtime().equal(off.runtime()) {
		t.Fatal("MySQL writes its binary log either way: turning point-in-time recovery on needs no restart")
	}
	pending := record{instanceSpec: ok, Applied: &runtimeConfig{}}
	if !pending.archiving() {
		t.Fatal("a pending restart does not hold up MySQL's binary log collection")
	}
	if args := strings.Join(record{instanceSpec: ok}.command(), " "); strings.Contains(args, "archive") {
		t.Fatalf("MySQL's command line does not change: %s", args)
	}
}

func TestADumpRecordsItsBinaryLogPosition(t *testing.T) {
	for text, want := range map[string]string{
		"-- MySQL dump\n--\n-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000002', SOURCE_LOG_POS=1180;\n": "binlog.000002:1180",
		"-- CHANGE MASTER TO MASTER_LOG_FILE='binlog.000123', MASTER_LOG_POS=157;\n":                                 "binlog.000123:157",
		"-- MySQL dump without a position\n":                                                   "",
		"-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='../etc/passwd', SOURCE_LOG_POS=4;\n": "",
	} {
		var out bytes.Buffer
		d := &dumpHead{w: &out}
		for chunk := range slices.Chunk([]byte(text), 7) { // the position may arrive split
			d.Write(chunk)
		}
		file, pos, ok := d.position()
		got := ""
		if ok {
			got = fmt.Sprintf("%s:%d", file, pos)
		}
		if got != want || out.String() != text {
			t.Errorf("%q: %q, passed %q", text, got, out.String())
		}
	}
}

func TestTheReplayStopsBeforeTheFirstCommitAfterTheTarget(t *testing.T) {
	t0 := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	data, starts, end := fakeBinlog(t0, t0.Add(time.Second), t0.Add(2*time.Second+400*time.Millisecond), t0.Add(3*time.Second))
	for _, c := range []struct {
		from   int64
		target time.Time
		cut    int64
	}{
		{4, t0.Add(2 * time.Second), starts[1]},                      // 10:00:02.4 is after 10:00:02
		{4, t0.Add(2*time.Second + 400*time.Millisecond), starts[2]}, // a commit at the target itself is kept
		{4, t0, starts[0]},           // before every commit
		{starts[2], t0, starts[2]},   // what the backup holds already is not replayed
		{4, t0.Add(time.Minute), -1}, // after every commit: replay all
	} {
		cut, gotEnd, err := scanBinlog(bytes.NewReader(data), c.from, c.target)
		if err != nil || cut != c.cut || gotEnd != end {
			t.Errorf("from %d to %v: cut %d end %d, want %d end %d (%v)", c.from, c.target, cut, gotEnd, c.cut, end, err)
		}
	}
	if _, _, err := scanBinlog(bytes.NewReader(data[:len(data)-10]), 4, t0); err == nil {
		t.Error("a truncated log is not replayed")
	}
	if _, _, err := scanBinlog(strings.NewReader("REDIS0011"), 4, t0); err == nil {
		t.Error("other data is not replayed")
	}
}

// A MySQL server's binary logs and how OpenRDS reached them.
type binlogServer struct {
	mu      sync.Mutex
	logs    []string
	data    map[string][]byte
	flushes int
	copied  []string
	next    int
}

func (s *binlogServer) run(_ context.Context, _ []string, in io.Reader, out io.Writer, args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if args[0] != "compose" || composeVerb(args) != "exec" {
		return nil
	}
	if i := slices.Index(args, binlogTarScript); i >= 0 {
		tw := tar.NewWriter(out)
		for _, name := range args[i+2:] {
			s.copied = append(s.copied, name)
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0640, Size: int64(len(s.data[name])), ModTime: time.Unix(1795672600, 0)})
			tw.Write(s.data[name])
		}
		return tw.Close()
	}
	sql, _ := io.ReadAll(in)
	switch {
	case strings.Contains(string(sql), "SHOW BINARY LOGS"):
		for _, name := range s.logs {
			fmt.Fprintf(out, "%s\t%d\tNo\n", name, len(s.data[name]))
		}
	case strings.Contains(string(sql), "FLUSH BINARY LOGS"):
		s.flushes++
		s.add()
	}
	return nil
}

func (s *binlogServer) add() {
	s.next++
	name := fmt.Sprintf("binlog.%06d", s.next)
	s.logs = append(s.logs, name)
	s.data[name] = []byte("header")
}

func (s *binlogServer) write(change string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := s.logs[len(s.logs)-1]
	s.data[open] = append(s.data[open], change...)
}

func mysqlBase(t *testing.T, m *manager, name, key, file string, pos int64, created time.Time) backup {
	t.Helper()
	b := backup{ID: randomHex(16), Name: name, Engine: "mysql", Version: "8.4", Kind: "automated", DataKey: key, Started: created.Add(-time.Minute),
		Created: created, Compression: "gzip", BinlogFile: file, BinlogPos: pos}
	dir := filepath.Join(m.backupDir(), b.ID)
	os.MkdirAll(dir, 0700)
	data := gz(t, []byte("CREATE TABLE t (id int);\n"))
	b.Bytes = int64(len(data))
	os.WriteFile(filepath.Join(dir, "data"), data, 0600)
	if err := writeJSON(filepath.Join(dir, "meta.json"), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClosedBinaryLogsAreCopiedOnce(t *testing.T) {
	s := &binlogServer{data: map[string][]byte{}}
	for range 3 {
		s.add()
	}
	m := testManager(t, s.run)
	ctx := t.Context()
	r := record{instanceSpec: instanceSpec{Name: "shop", Engine: "mysql", PITR: true, BackupRetentionDays: 7}, Project: "openrds-db-shop"}
	if n, err := m.collectBinlogs(ctx, r); n != 0 || err != nil || s.flushes != 0 {
		t.Fatalf("before the first base backup nothing is collected: %d %v, %d flushes", n, err, s.flushes)
	}
	mysqlBase(t, m, "shop", r.diskKey(), "binlog.000002", 300, time.Now().UTC())

	// The log the backup's position is in, and later ones, are copied once closed.
	n, err := m.collectBinlogs(ctx, r)
	if err != nil || n != 2 || s.flushes != 1 || strings.Join(s.copied, " ") != "binlog.000002 binlog.000003" {
		t.Fatalf("collected %d (%v), %d flushes, copied %v", n, err, s.flushes, s.copied)
	}
	files, _ := m.walFiles(r.diskKey())
	if len(files) != 2 || files[0].Name != "binlog.000002" || files[1].At.Unix() != 1795672600 {
		t.Fatalf("stored: %+v", files)
	}

	// A quiet database is not flushed again, and is covered up to now.
	before := time.Now().UTC()
	if n, err := m.collectBinlogs(ctx, r); n != 0 || err != nil || s.flushes != 1 {
		t.Fatalf("a quiet round: %d %v, %d flushes", n, err, s.flushes)
	}
	if latest := m.latestWAL(r.diskKey()); latest.Before(before) {
		t.Fatalf("every change is in the library, so the window reaches now: %v", latest)
	}

	// A change gets its log closed and copied; earlier logs are not copied again.
	s.write("insert")
	s.copied = nil
	if n, err := m.collectBinlogs(ctx, r); n != 1 || err != nil || s.flushes != 2 || strings.Join(s.copied, " ") != "binlog.000004" {
		t.Fatalf("after a change: %d %v, %d flushes, copied %v", n, err, s.flushes, s.copied)
	}

	// Logs MySQL removed before they were copied are reported.
	s.mu.Lock()
	s.logs = s.logs[len(s.logs)-1:]
	s.add()
	s.add()
	s.logs = slices.Delete(s.logs, 0, 1) // binlog.000005 is gone
	s.mu.Unlock()
	s.write("insert")
	if _, err := m.collectBinlogs(ctx, r); err != nil {
		t.Fatal(err)
	}
	items, _ := m.readActivity()
	if items[0].Action != "Collect binary logs for point-in-time recovery" || items[0].Result != "failed" || !strings.Contains(items[0].Detail, "from number 5 to 5") {
		t.Fatalf("events: %+v", items[0])
	}
	mark, _ := os.ReadFile(filepath.Join(m.walDir(r.diskKey()), binlogMarkFile))
	if strings.TrimSpace(string(mark)) != "binlog.000007" {
		t.Fatalf("the last copied log is remembered: %q", mark)
	}
}

func storeBinlog(t *testing.T, m *manager, key, name string, at time.Time, data []byte) {
	t.Helper()
	dir := m.walDir(key)
	os.MkdirAll(dir, 0700)
	if err := storeWAL(dir, at, name, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestTheBinaryLogsARestoreNeedsFollowEachOther(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	b := mysqlBase(t, m, "shop", "openrds-db-shop", "binlog.000003", 400, day)
	for i, h := range []int{-1, 1, 2, 3, 5} { // binlog.000001 to binlog.000005, each closed at that hour
		storeBinlog(t, m, b.DataKey, fmt.Sprintf("binlog.%06d", i+1), day.Add(time.Duration(h)*time.Hour), []byte("log"))
	}
	files, _, err := m.binlogsForRestore(t.Context(), b, day.Add(150*time.Minute))
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if err != nil || strings.Join(names, " ") != "binlog.000003 binlog.000004" {
		t.Fatalf("from the backup's log to the first one closed after the target: %v %v", names, err)
	}
	os.Remove(filepath.Join(m.walDir(b.DataKey), files[1].Stored))
	if _, _, err := m.binlogsForRestore(t.Context(), b, day.Add(4*time.Hour)); status(err) != 409 || !strings.Contains(err.Error(), "binlog.000004 is missing") {
		t.Fatalf("a gap is refused: %v", err)
	}
	if files, _, err := m.binlogsForRestore(t.Context(), b, day.Add(30*time.Minute)); err != nil || len(files) != 1 {
		t.Fatalf("a time soon after the backup needs its own log only: %v %v", files, err)
	}
}

func TestAMySQLRestoreReplaysBinaryLogsToTheTarget(t *testing.T) {
	f := newFakeDocker()
	var mu sync.Mutex
	var statements, relayed []string
	run := func(ctx context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
		if args[0] != "compose" || composeVerb(args) != "exec" {
			return f.run(ctx, env, in, out, args...)
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case slices.Contains(args, relayLogScript):
			i := slices.Index(args, relayLogScript)
			if args[i+2] != "/var/lib/mysql/abc-relay-bin" {
				return fmt.Errorf("relay log %q", args[i+2])
			}
			tr := tar.NewReader(in)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				relayed = append(relayed, h.Name)
			}
			io.Copy(io.Discard, in)
			return nil
		case slices.Contains(args, mysqlStatusClient):
			fmt.Fprint(out, "Replica_IO_Running\tReplica_SQL_Running\tLast_SQL_Errno\tLast_SQL_Error\nNo\tNo\t0\t\n")
			return nil
		case slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "exec mysql -u root -N -B") }):
			sql, _ := io.ReadAll(in)
			statements = append(statements, string(sql))
			switch {
			case strings.Contains(string(sql), "@@relay_log_basename"):
				fmt.Fprintln(out, "/var/lib/mysql/abc-relay-bin")
			case strings.Contains(string(sql), "information_schema.EVENTS"):
				fmt.Fprintln(out, "ALTER EVENT `app`.`tick` ENABLE;")
			}
			return nil
		}
		return f.run(ctx, env, in, out, args...)
	}
	m := testManager(t, run)
	ctx := t.Context()
	day := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	src := record{instanceSpec: instanceSpec{Name: "shop", Engine: "mysql", Version: "8.4", Port: 13306, BackupRetentionDays: 7, PITR: true, MemoryMiB: 1024}, Project: "openrds-db-shop"}
	writeJSON(m.recordPath(src.Name), src)
	first, firstStarts, _ := fakeBinlog(day, day.Add(30*time.Second), day.Add(90*time.Second))
	second, secondStarts, _ := fakeBinlog(day.Add(2*time.Minute), day.Add(150*time.Second), day.Add(170*time.Second))
	mysqlBase(t, m, "shop", src.diskKey(), "binlog.000007", firstStarts[1], day.Add(time.Minute))
	storeBinlog(t, m, src.diskKey(), "binlog.000007", day.Add(2*time.Minute), first)
	storeBinlog(t, m, src.diskKey(), "binlog.000008", day.Add(3*time.Minute), second)

	err := m.restoreToTime(ctx, "shop", restoreToTimeRequest{Name: "shop-at", Port: 13307, TargetTime: day.Add(160 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := m.record("shop-at")
	if r.PendingRestore || r.MemoryMiB != 1024 || r.PITR || r.Version != "8.4" {
		t.Fatalf("the new database has the source's size and version: %+v", r)
	}
	if strings.Join(relayed, " ") != "binlog.000007.gz binlog.000008.gz" {
		t.Fatalf("the logs from the backup's position to the target: %v", relayed)
	}
	all := strings.Join(statements, "\n")
	for _, want := range []string{
		"SET GLOBAL event_scheduler = OFF;",
		fmt.Sprintf("CHANGE REPLICATION SOURCE TO RELAY_LOG_FILE = 'abc-relay-bin.000007', RELAY_LOG_POS = %d, SOURCE_HOST = 'openrds-restore';", firstStarts[1]),
		"CHANGE REPLICATION FILTER REPLICATE_WILD_IGNORE_TABLE = ('mysql.%');",
		fmt.Sprintf("START REPLICA SQL_THREAD UNTIL RELAY_LOG_FILE = 'abc-relay-bin.000008', RELAY_LOG_POS = %d;", secondStarts[1]),
		"RESET REPLICA ALL;",
		"ALTER EVENT `app`.`tick` ENABLE;\nSET GLOBAL event_scheduler = ON;",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the restore lacks %q", want)
		}
	}
	if !strings.Contains(all, "SET GLOBAL server_id = ") || strings.Contains(all, "SET GLOBAL server_id = 1;") {
		t.Error("the applier runs with a server ID of its own, so it does not skip the source's events")
	}
	f.mu.Lock()
	loaded := strings.Join(f.inputs, "\n")
	f.mu.Unlock()
	if !strings.Contains(loaded, "CREATE TABLE t") {
		t.Fatal("the base backup is loaded first")
	}
	items, _ := m.readActivity()
	if items[0].Action != "Restore to point in time" || items[0].Result != "completed" || !strings.Contains(items[0].Detail, "shop as of 2026-03-09 10:02:40 UTC") {
		t.Fatalf("events: %+v", items[0])
	}

	// A target before the next commit replays nothing.
	statements = nil
	if err := m.restoreToTime(ctx, "shop", restoreToTimeRequest{Name: "shop-early", Port: 13308, TargetTime: day.Add(80 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(statements, "\n"); strings.Contains(all, "START REPLICA") || !strings.Contains(all, "event_scheduler = ON") {
		t.Fatalf("nothing committed between the backup and the target: %s", all)
	}
}

func TestAMySQLBaseBackupRecordsItsPosition(t *testing.T) {
	f := newFakeDocker()
	position := "-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='binlog.000004', SOURCE_LOG_POS=1180;\n"
	var scripts []string
	m := testManager(t, func(ctx context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
		if args[0] == "compose" && composeVerb(args) == "exec" && slices.Contains(args, mysqlBaseDump) {
			scripts = append(scripts, args[len(args)-1])
			fmt.Fprint(out, "-- MySQL dump\n"+position+"CREATE TABLE t (id int);\n")
			return nil
		}
		return f.run(ctx, env, in, out, args...)
	})
	r := record{instanceSpec: instanceSpec{Name: "shop", Engine: "mysql", Port: 13306, BackupRetentionDays: 7, PITR: true}, Project: "openrds-db-shop"}
	writeJSON(m.recordPath(r.Name), r)
	f.running[r.Project] = true
	b, err := m.makeBackup(t.Context(), "shop", "automated")
	if err != nil {
		t.Fatal(err)
	}
	if b.BinlogFile != "binlog.000004" || b.BinlogPos != 1180 || b.DataKey != r.diskKey() || b.Format != "" || !b.pitrBase() || b.Compression != "gzip" {
		t.Fatalf("an automated backup with point-in-time recovery on: %+v", b)
	}
	if !strings.Contains(scripts[0], "--source-data=2") || !strings.Contains(scripts[0], "lock_wait_timeout = 5") {
		t.Fatalf("the dump records its position without holding up writes for long: %s", scripts[0])
	}
	if b, err := m.makeBackup(t.Context(), "shop", "manual"); err != nil || b.pitrBase() {
		t.Fatalf("a snapshot stays a plain dump: %+v %v", b, err)
	}
	position = ""
	if _, err := m.makeBackup(t.Context(), "shop", "automated"); status(err) != 502 || !strings.Contains(err.Error(), "binary log position") {
		t.Fatalf("a dump without a position is no base backup: %v", err)
	}
}

func TestNewDataGetsItsFirstBaseBackupAtOnce(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	now := time.Date(2026, 3, 9, 10, 20, 0, 0, time.UTC)
	r := record{instanceSpec: instanceSpec{Name: "shop", Engine: "mysql", Port: 13306, BackupRetentionDays: 7, PITR: true, BackupWindow: "03:00-03:30"},
		Project: "openrds-db-shop", Created: now.Add(-48 * time.Hour)}
	writeJSON(m.recordPath(r.Name), r)
	f.running[r.Project] = true
	if due := m.dueBackups(t.Context(), now); !slices.Equal(due, []string{"shop"}) {
		t.Fatalf("point-in-time recovery starts with a base backup: %v", due)
	}
	mysqlBase(t, m, "shop", r.diskKey(), "binlog.000002", 158, now.Add(time.Minute))
	if due := m.dueBackups(t.Context(), now.Add(2*time.Minute)); len(due) != 0 {
		t.Fatalf("one base backup is enough: %v", due)
	}
	r.DataGeneration = 1 // an upgrade in the same hour
	writeJSON(m.recordPath(r.Name), r)
	if due := m.dueBackups(t.Context(), now.Add(5*time.Minute)); !slices.Equal(due, []string{"shop"}) {
		t.Fatalf("the upgraded data's first base backup does not wait for the next hour: %v", due)
	}
	if due := m.dueBackups(t.Context(), now.Add(6*time.Minute)); len(due) != 0 {
		t.Fatalf("a failed first try waits an hour: %v", due)
	}
}
