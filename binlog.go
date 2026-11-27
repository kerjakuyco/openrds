package main

// Point-in-time recovery for MySQL, like RDS: MySQL writes every change to
// its binary log anyway. With point-in-time recovery on, OpenRDS closes the
// open binary log at least once a minute when it holds new changes, copies
// the closed logs into the backup library and backup storage, and automated
// backups record the binary log position they were taken at. Restoring to a
// time loads the newest backup before it into a new database, whose own
// replication applier then replays the logs up to that second.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The last binary log copied for a data key, so logs already pruned from the
// library are never copied again.
const binlogMarkFile = ".binlog-collected"

var (
	binlogNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[0-9]{6,}$`)
	relayPathPattern  = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	// mysqldump --source-data=2 writes the position as a comment near the top.
	dumpPositionPattern = regexp.MustCompile(`CHANGE (?:REPLICATION SOURCE|MASTER) TO (?:SOURCE|MASTER)_LOG_FILE='([^']+)', (?:SOURCE|MASTER)_LOG_POS=([0-9]+)`)
)

// A binary log's sequence number: binlog.000042 is 42.
func binlogSeq(name string) (int64, bool) {
	if !binlogNamePattern.MatchString(name) {
		return 0, false
	}
	n, err := strconv.ParseInt(name[strings.LastIndexByte(name, '.')+1:], 10, 64)
	return n, err == nil
}

// ---------- base backups

// Automated backups with point-in-time recovery on: a consistent dump that
// records its binary log position. Recording it takes a moment of global read
// lock, which waits for running queries and holds up writes meanwhile, so the
// backup does not start behind a long query and gives up after 5 seconds; the
// scheduler tries again later.
const mysqlBaseDump = `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
long=$(mysql -u root -N -B -e "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE COMMAND = 'Query' AND TIME >= 5 AND ID <> CONNECTION_ID()")
if [ "$long" != 0 ]; then
  echo "A query has run for over 5 seconds. The backup waits for none, so it does not hold up writes; it is tried again later." >&2
  exit 3
fi
exec mysqldump -u root --single-transaction --quick --no-tablespaces --source-data=2 --set-gtid-purged=OFF --routines --events --triggers --hex-blob --init-command="SET SESSION lock_wait_timeout = 5" app`

// Passes a dump through, keeping its start, where mysqldump writes the
// binary log position.
type dumpHead struct {
	w    io.Writer
	head []byte
}

func (d *dumpHead) Write(p []byte) (int, error) {
	if room := 64<<10 - len(d.head); room > 0 {
		d.head = append(d.head, p[:min(room, len(p))]...)
	}
	return d.w.Write(p)
}

func (d *dumpHead) position() (string, int64, bool) {
	m := dumpPositionPattern.FindSubmatch(d.head)
	if m == nil {
		return "", 0, false
	}
	pos, err := strconv.ParseInt(string(m[2]), 10, 64)
	if _, ok := binlogSeq(string(m[1])); !ok || err != nil || pos < 4 {
		return "", 0, false
	}
	return string(m[1]), pos, true
}

// ---------- collection

type binlogInfo struct {
	Name string
	Seq  int64
	Size int64
}

// The server's binary logs, oldest first; the last one is open.
func (m *manager) binaryLogs(ctx context.Context, r record) ([]binlogInfo, error) {
	out, err := m.query(ctx, r, "SHOW BINARY LOGS;\n")
	if err != nil {
		if strings.Contains(err.Error(), "not using binary logging") {
			return nil, errors.New("binary logging is off")
		}
		return nil, err
	}
	var logs []binlogInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		seq, ok := binlogSeq(f[0])
		size, err := strconv.ParseInt(f[1], 10, 64)
		if ok && err == nil {
			logs = append(logs, binlogInfo{f[0], seq, size})
		}
	}
	if len(logs) == 0 {
		return nil, errors.New("binary logging is off")
	}
	slices.SortFunc(logs, func(a, b binlogInfo) int { return cmp.Compare(a.Seq, b.Seq) })
	return logs, nil
}

// Whether the open binary log is as it was right after OpenRDS closed the
// previous one: then it holds no changes yet.
func (p *pitrState) unchanged(dataKey string, open binlogInfo) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.flushed[dataKey]
	return ok && f == open
}

func (p *pitrState) noteFlush(dataKey string, open binlogInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.flushed == nil {
		p.flushed = make(map[string]binlogInfo)
	}
	p.flushed[dataKey] = open
}

const binlogTarScript = `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
base=$(mysql -u root -N -B -e 'SELECT @@log_bin_basename')
cd "${base%/*}"
exec tar -cf - -- "$@"`

// Close the open binary log when it holds changes, then copy closed logs
// into the library, oldest first, from the one the oldest base backup of the
// data starts in. MySQL keeps its own copies for binlog_expire_logs_seconds.
func (m *manager) collectBinlogs(ctx context.Context, r record) (int, error) {
	key := r.diskKey()
	backups, err := m.backups()
	if err != nil {
		return 0, err
	}
	from := int64(-1)
	for _, b := range backups {
		if seq, ok := binlogSeq(b.BinlogFile); ok && b.DataKey == key && (from < 0 || seq < from) {
			from = seq
		}
	}
	if from < 0 {
		return 0, nil // nothing to replay onto before the first base backup
	}
	dir := m.walDir(key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	expected := from
	mark, _ := os.ReadFile(filepath.Join(dir, binlogMarkFile))
	if seq, ok := binlogSeq(strings.TrimSpace(string(mark))); ok && seq >= from {
		expected = seq + 1
	}
	checked := time.Now().UTC()
	logs, err := m.binaryLogs(ctx, r)
	if err != nil {
		return 0, err
	}
	quiet := m.pitr.unchanged(key, logs[len(logs)-1])
	if !quiet {
		if err := m.admin(ctx, r, "FLUSH BINARY LOGS;\n"); err != nil {
			return 0, err
		}
		if logs, err = m.binaryLogs(ctx, r); err != nil {
			return 0, err
		}
		m.pitr.noteFlush(key, logs[len(logs)-1])
	}
	var names []string
	for _, l := range logs[:len(logs)-1] {
		if l.Seq >= expected && len(names) < walBatch {
			names = append(names, l.Name)
		}
	}
	if len(names) == 0 {
		if quiet {
			m.noteLatestWAL(key, checked) // every change so far is in the library
		}
		return 0, nil
	}
	if first, _ := binlogSeq(names[0]); first > expected {
		m.note(ctx, r.Name, "Collect binary logs for point-in-time recovery", "failed",
			fmt.Sprintf("MySQL removed binary logs before OpenRDS copied them, from number %d to %d. Times between the backup before them and the next one can’t be restored.", expected, first-1))
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := m.compose(ctx, r, nil, pw, append([]string{"exec", "-T", "db", "sh", "-ec", binlogTarScript, "openrds"}, names...)...)
		pw.CloseWithError(err)
		done <- err
	}()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var stored []string
	var newest time.Time
	var readErr error
	tr := tar.NewReader(pr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		if h.Typeflag != tar.TypeReg || !want[h.Name] {
			continue
		}
		at := h.ModTime.UTC()
		if err := storeWAL(dir, at, h.Name, tr); err != nil {
			readErr = err
			break
		}
		stored, newest = append(stored, h.Name), at
	}
	io.Copy(io.Discard, pr)
	if err := <-done; err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	if len(stored) != len(names) {
		return 0, fmt.Errorf("copied %d of %d binary logs", len(stored), len(names))
	}
	if err := writeFile(filepath.Join(dir, binlogMarkFile), []byte(stored[len(stored)-1]+"\n")); err != nil {
		return len(stored), err
	}
	m.noteLatestWAL(key, newest)
	if quiet && len(names) < walBatch {
		m.noteLatestWAL(key, checked)
	}
	return len(stored), nil
}

// ---------- restore to a time

// The binary logs a restore to target replays, oldest first: from the one
// the base backup's position is in, to the first one closed after the
// target. They must follow each other without a gap.
func (m *manager) binlogsForRestore(ctx context.Context, b backup, target time.Time) ([]walFile, func(walFile) (io.ReadCloser, int64, error), error) {
	first, ok := binlogSeq(b.BinlogFile)
	if !ok {
		return nil, nil, fail(400, "This backup has no binary log position, so it can’t be rolled forward.")
	}
	all, open, err := m.storedLogs(ctx, b.DataKey)
	if err != nil {
		return nil, nil, err
	}
	bySeq := map[int64]walFile{}
	var seqs []int64
	for _, f := range all {
		seq, ok := binlogSeq(f.Name)
		if _, seen := bySeq[seq]; ok && !seen && seq >= first {
			bySeq[seq] = f
			seqs = append(seqs, seq)
		}
	}
	slices.Sort(seqs)
	prefix := b.BinlogFile[:strings.LastIndexByte(b.BinlogFile, '.')]
	var picked []walFile
	for i, seq := range seqs {
		if want := first + int64(i); seq != want {
			return nil, nil, fail(409, fmt.Sprintf("Binary log %s.%06d is missing, so this time can’t be restored. Choose a time after the next backup.", prefix, want))
		}
		f := bySeq[seq]
		picked = append(picked, f)
		if f.At.After(target) {
			break
		}
	}
	return picked, open, nil
}

const (
	stopEvent          = 3
	rotateEvent        = 4
	gtidEvent          = 33
	anonymousGTIDEvent = 34
	gtidTaggedEvent    = 42
)

// Where to stop replaying one binary log: the offset of the first transaction
// at or after from that committed after target, or -1, and where its last
// transaction ends. A binary log is a magic number, then events with a
// 19-byte header; every transaction starts with a GTID event (anonymous while
// GTIDs are off) that carries its commit time in microseconds.
func scanBinlog(r io.Reader, from int64, target time.Time) (cut, end int64, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || magic != [4]byte{0xfe, 'b', 'i', 'n'} {
		return 0, 0, errors.New("it is not a MySQL binary log")
	}
	limit := target.UnixMicro()
	off, cut, end := int64(4), int64(-1), int64(4)
	var head [19]byte
	for {
		if _, err := io.ReadFull(br, head[:]); err == io.EOF {
			return cut, end, nil
		} else if err != nil {
			return 0, 0, fmt.Errorf("it ends inside an event at %d", off)
		}
		kind := head[4]
		size := int64(binary.LittleEndian.Uint32(head[9:]))
		if size < int64(len(head)) {
			return 0, 0, fmt.Errorf("it has a damaged event at %d", off)
		}
		rest := size - int64(len(head))
		committed := int64(binary.LittleEndian.Uint32(head[0:])) * 1e6
		if kind == gtidEvent || kind == anonymousGTIDEvent {
			body := make([]byte, min(rest, 64))
			if _, err := io.ReadFull(br, body); err != nil {
				return 0, 0, fmt.Errorf("it ends inside an event at %d", off)
			}
			if t, ok := gtidCommitTime(body); ok {
				committed = t
			}
			rest -= int64(len(body))
		}
		if _, err := io.CopyN(io.Discard, br, rest); err != nil {
			return 0, 0, fmt.Errorf("it ends inside an event at %d", off)
		}
		if cut < 0 && off >= from && (kind == gtidEvent || kind == anonymousGTIDEvent || kind == gtidTaggedEvent) && committed > limit {
			cut = off
		}
		if kind != rotateEvent && kind != stopEvent {
			end = off + size
		}
		off += size
	}
}

// The commit time in a GTID event: flags, source UUID and number, then
// logical timestamps, then the commit time on this server in 7 bytes, its top
// bit saying whether the original server's time follows.
func gtidCommitTime(body []byte) (int64, bool) {
	const at = 1 + 16 + 8 + 1 + 16
	if len(body) < at+7 || body[at-17] != 2 {
		return 0, false
	}
	var raw uint64
	for i := range 7 {
		raw |= uint64(body[at+i]) << (8 * i)
	}
	raw &^= 1 << 55
	return int64(raw), raw != 0
}

// Stream binary logs as a tar of their gzip files, each read once more on
// the way to find where to stop.
func writeBinlogTar(w io.Writer, files []walFile, open func(walFile) (io.ReadCloser, int64, error), scan func(i int, r io.Reader) error) error {
	tw := tar.NewWriter(w)
	for i, f := range files {
		body, size, err := open(f)
		if err != nil {
			return err
		}
		err = func() error {
			defer body.Close()
			if err := tw.WriteHeader(&tar.Header{Name: f.Name + ".gz", Mode: 0600, Size: size, ModTime: f.At}); err != nil {
				return err
			}
			tee := io.TeeReader(body, tw)
			z, err := gzip.NewReader(tee)
			if err != nil {
				return fmt.Errorf("binary log %s is damaged: %w", f.Name, err)
			}
			if err := scan(i, z); err != nil {
				return fmt.Errorf("binary log %s: %w", f.Name, err)
			}
			_, err = io.Copy(io.Discard, tee)
			return err
		}()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// The binary logs become relay logs of the new database: links named after
// its relay log, listed in its index, so its applier reads them.
const relayLogScript = `relay=$1
shift
rm -rf /tmp/openrds-binlog
mkdir -p /tmp/openrds-binlog
cd /tmp/openrds-binlog
tar -xf -
: > "$relay.index"
for f in "$@"; do
  gunzip "$f.gz"
  ln -s "/tmp/openrds-binlog/$f" "$relay.${f##*.}"
  echo "./${relay##*/}.${f##*.}" >> "$relay.index"
done
chown -R mysql:mysql /tmp/openrds-binlog "$relay.index"
chown -h mysql:mysql "$relay".[0-9]*`

// Replay binary logs into a new database up to target, with its own
// replication applier: account changes are filtered out (users belong to the
// source), and the applier stops before the first transaction committed after
// the target.
func (m *manager) replayBinlogs(ctx context.Context, r record, b backup, target time.Time) error {
	files, open, err := m.binlogsForRestore(ctx, b, target)
	if err != nil || len(files) == 0 {
		return err // without logs after the backup, nothing changed since
	}
	out, err := m.query(ctx, r, "SELECT @@relay_log_basename;\n")
	relay := strings.TrimSpace(out)
	if err != nil {
		return err
	}
	if !relayPathPattern.MatchString(relay) {
		return fmt.Errorf("unexpected relay log path %q", relay)
	}
	type position struct {
		file int
		pos  int64
	}
	until, last := position{-1, 0}, position{}
	scan := func(i int, z io.Reader) error {
		from := int64(4)
		if i == 0 {
			from = b.BinlogPos
		}
		cut, end, err := scanBinlog(z, from, target)
		if err != nil {
			return err
		}
		if until.file < 0 && cut >= 0 {
			until = position{i, cut}
		}
		last = position{i, end}
		return nil
	}
	pr, pw := io.Pipe()
	scanned := make(chan error, 1)
	go func() {
		err := writeBinlogTar(pw, files, open, scan)
		pw.CloseWithError(err)
		scanned <- err
	}()
	args := []string{"exec", "-T", "db", "bash", "-ec", relayLogScript, "openrds", relay}
	for _, f := range files {
		args = append(args, f.Name)
	}
	err = m.compose(ctx, r, pr, nil, args...)
	pr.CloseWithError(errors.New("the copy ended"))
	if streamErr := <-scanned; streamErr != nil {
		return fmt.Errorf("reading binary logs failed: %w", streamErr)
	}
	if err != nil {
		return fmt.Errorf("copying binary logs failed: %w", err)
	}
	defer m.compose(context.WithoutCancel(ctx), r, nil, nil, "exec", "-T", "db", "rm", "-rf", "/tmp/openrds-binlog")
	if until.file < 0 {
		until = last
	}
	if until.file == 0 && until.pos <= b.BinlogPos {
		return nil // nothing committed between the backup and the target
	}
	relayFile := func(i int) string {
		name := files[i].Name
		return relay[strings.LastIndexByte(relay, '/')+1:] + name[strings.LastIndexByte(name, '.'):]
	}
	// Events carry the source's server ID, which the applier skips as its own.
	start := fmt.Sprintf(`SET GLOBAL server_id = %d;
CHANGE REPLICATION SOURCE TO RELAY_LOG_FILE = '%s', RELAY_LOG_POS = %d, SOURCE_HOST = 'openrds-restore';
CHANGE REPLICATION FILTER REPLICATE_WILD_IGNORE_TABLE = ('mysql.%%');
START REPLICA SQL_THREAD UNTIL RELAY_LOG_FILE = '%s', RELAY_LOG_POS = %d;
`, serverID(), relayFile(0), b.BinlogPos, relayFile(until.file), until.pos)
	if err := m.admin(ctx, r, start); err != nil {
		return fmt.Errorf("starting the replay failed: %w", err)
	}
	if err := m.awaitReplay(ctx, r); err != nil {
		return err
	}
	return m.admin(ctx, r, "CHANGE REPLICATION FILTER REPLICATE_WILD_IGNORE_TABLE = ();\nRESET REPLICA ALL;\n")
}

// SHOW REPLICA STATUS as names and values.
func replicaFields(out string) (map[string]string, bool) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		return nil, false
	}
	names, values := strings.Split(lines[0], "\t"), strings.Split(lines[1], "\t")
	v := map[string]string{}
	for i, n := range names {
		if i < len(values) {
			v[n] = values[i]
		}
	}
	return v, true
}

// Wait until the applier stops: at its stop position, or at an error.
func (m *manager) awaitReplay(ctx context.Context, r record) error {
	for {
		var out bytes.Buffer
		if err := m.compose(ctx, r, strings.NewReader("SHOW REPLICA STATUS;\n"), &out, "exec", "-T", "db", "sh", "-ec", mysqlStatusClient); err != nil {
			return err
		}
		v, ok := replicaFields(out.String())
		if !ok {
			return errors.New("the replay stopped reporting its progress")
		}
		if n := v["Last_SQL_Errno"]; n != "" && n != "0" {
			return fmt.Errorf("replaying binary logs failed: %s", strings.ReplaceAll(v["Last_SQL_Error"], `\n`, " "))
		}
		if v["Replica_SQL_Running"] == "No" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Replication creates events disabled on the replica; these were created
// while replaying.
const replayedEventsSQL = "SELECT CONCAT('ALTER EVENT `', REPLACE(EVENT_SCHEMA, '`', '``'), '`.`', REPLACE(EVENT_NAME, '`', '``'), '` ENABLE;') " +
	"FROM information_schema.EVENTS WHERE STATUS IN ('REPLICA_SIDE_DISABLED', 'SLAVESIDE_DISABLED');\n"

// Load a MySQL base backup, then replay binary logs to target. Events wait
// meanwhile: what they changed is in the logs already.
func (m *manager) restoreMySQLToTime(ctx context.Context, r record, input io.Reader, b backup, target time.Time) error {
	if err := m.admin(ctx, r, "SET GLOBAL event_scheduler = OFF;\n"); err != nil {
		return err
	}
	if err := m.restoreInto(ctx, r, input); err != nil {
		return err
	}
	if err := m.replayBinlogs(ctx, r, b, target); err != nil {
		return err
	}
	out, err := m.query(ctx, r, replayedEventsSQL)
	if err != nil {
		return err
	}
	return m.admin(ctx, r, strings.TrimSpace(out)+"\nSET GLOBAL event_scheduler = ON;\n")
}
