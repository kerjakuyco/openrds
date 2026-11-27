package main

// Point-in-time recovery for PostgreSQL, like RDS: PostgreSQL archives every
// finished WAL segment (at least once a minute) to its own volume, OpenRDS
// collects the segments into the backup library and backup storage, and
// automated backups become physical base backups. Restoring to a time creates
// a new database from the base backup before it and replays WAL up to that
// second. MySQL's binary logs take the same path; see binlog.go.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// The longest a quiet database waits before archiving its current WAL
	// segment, and so roughly how much a restore can miss.
	archiveTimeout = 60
	// Archive complete files only, and never overwrite one.
	archiveCommand = `test ! -f /archive/%f && cp %p /archive/%f.tmp && mv /archive/%f.tmp /archive/%f`
	// Segments collected per round; a busy database catches up over rounds.
	walBatch = 64
)

// pg_basebackup over the local socket: the whole cluster as one tar, with the
// WAL it needs to be consistent by itself.
var baseBackupCommand = []string{"pg_basebackup", "-U", "openrds", "-D", "-", "-Ft", "-X", "fetch", "-c", "fast"}

var walNamePattern = regexp.MustCompile(`^([0-9A-F]{24}(\.partial|\.[0-9A-F]{8}\.backup)?|[0-9A-F]{8}\.history)$`)

type pitrState struct {
	collecting sync.Mutex
	mu         sync.Mutex
	latest     map[string]time.Time  // newest archived WAL or binary log by data key
	failures   map[string]int        // consecutive failed collections by project
	flushed    map[string]binlogInfo // MySQL: the open binary log right after OpenRDS closed the one before
}

// Whether the database's logs are collected: for PostgreSQL, whether the
// running container archives WAL, so its applied settings while a change
// waits for a restart. MySQL writes its binary log either way.
func (r record) archiving() bool {
	if r.Engine != "mysql" && r.Applied != nil {
		return r.Applied.PITR
	}
	return r.PITR
}

func checkPITR(s instanceSpec) error {
	if !s.PITR {
		return nil
	}
	if s.Engine != "postgres" && s.Engine != "mysql" {
		return fail(400, "Point-in-time recovery is available for PostgreSQL and MySQL.")
	}
	if s.BackupRetentionDays == 0 {
		return fail(400, "Turn on automated backups first: point-in-time recovery reaches back as far as they are kept.")
	}
	if v, ok := s.Parameters["binlog_expire_logs_seconds"]; ok && s.Engine == "mysql" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n != 0 && n < 86400 {
			return fail(400, "With point-in-time recovery on, keep binary logs for at least a day (binlog_expire_logs_seconds of 86400 or more, or 0), so none is removed before OpenRDS copies it.")
		}
	}
	return nil
}

// A backup that point-in-time recovery replays logs onto: a PostgreSQL base
// backup, or a MySQL dump that records its binary log position.
func (b backup) pitrBase() bool { return b.Format == "physical" || b.BinlogFile != "" }

// ---------- the WAL store: <library>/pitr/<data key>/<archived unix>_<name>.gz

type walFile struct {
	At     time.Time
	Name   string // PostgreSQL's or MySQL's file name
	Stored string // the library's file name, with the time it was archived
}

func (m *manager) walDir(dataKey string) string { return filepath.Join(m.dir, "pitr", dataKey) }

func parseWAL(stored string) (walFile, bool) {
	base, ok := strings.CutSuffix(stored, ".gz")
	if !ok {
		return walFile{}, false
	}
	unix, name, ok := strings.Cut(base, "_")
	n, err := strconv.ParseInt(unix, 10, 64)
	if !ok || err != nil || !walNamePattern.MatchString(name) && !binlogNamePattern.MatchString(name) {
		return walFile{}, false
	}
	return walFile{At: time.Unix(n, 0).UTC(), Name: name, Stored: stored}, true
}

func sortWAL(files []walFile) {
	slices.SortFunc(files, func(a, b walFile) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}

func (m *manager) walFiles(dataKey string) ([]walFile, error) {
	entries, err := os.ReadDir(m.walDir(dataKey))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []walFile
	for _, e := range entries {
		if f, ok := parseWAL(e.Name()); ok {
			files = append(files, f)
		}
	}
	sortWAL(files)
	return files, nil
}

// The newest archived WAL of a data key, remembered between collections.
func (m *manager) latestWAL(dataKey string) time.Time {
	m.pitr.mu.Lock()
	t, ok := m.pitr.latest[dataKey]
	m.pitr.mu.Unlock()
	if ok {
		return t
	}
	files, _ := m.walFiles(dataKey)
	if len(files) > 0 { // nothing found is not remembered: a first segment may arrive any time
		t = files[len(files)-1].At
		m.noteLatestWAL(dataKey, t)
	}
	return t
}

func (m *manager) noteLatestWAL(dataKey string, t time.Time) {
	m.pitr.mu.Lock()
	defer m.pitr.mu.Unlock()
	if m.pitr.latest == nil {
		m.pitr.latest = make(map[string]time.Time)
	}
	if t.After(m.pitr.latest[dataKey]) {
		m.pitr.latest[dataKey] = t
	}
}

// ---------- collection

const walListScript = `cd /archive 2>/dev/null || exit 0
ls -tr | grep -v '\.tmp$' | head -n %d > /tmp/openrds-wal || true
[ -s /tmp/openrds-wal ] || exit 0
exec tar -cf - -T /tmp/openrds-wal`

// Move archived WAL from the database's archive volume into the library,
// oldest first. Files leave the volume only once they are stored.
func (m *manager) collectWAL(ctx context.Context, r record) (int, error) {
	dir := m.walDir(r.diskKey())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := m.compose(ctx, r, nil, pw, "exec", "-T", "db", "sh", "-ec", fmt.Sprintf(walListScript, walBatch))
		pw.CloseWithError(err)
		done <- err
	}()
	var names []string
	var newest time.Time
	tr := tar.NewReader(pr)
	var readErr error
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		if h.Typeflag != tar.TypeReg || !walNamePattern.MatchString(h.Name) {
			continue
		}
		at := h.ModTime.UTC()
		if err := storeWAL(dir, at, h.Name, tr); err != nil {
			readErr = err
			break
		}
		names = append(names, h.Name)
		newest = at
	}
	io.Copy(io.Discard, pr)
	if err := <-done; err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	if len(names) == 0 {
		return 0, nil
	}
	if err := m.compose(ctx, r, strings.NewReader(strings.Join(names, "\n")+"\n"), nil,
		"exec", "-T", "db", "sh", "-ec", `cd /archive && xargs rm -f --`); err != nil {
		return len(names), err // stored; the next round stores them again harmlessly
	}
	m.noteLatestWAL(r.diskKey(), newest)
	return len(names), nil
}

// Write one segment gzip-compressed, under a temporary name until complete.
func storeWAL(dir string, at time.Time, name string, data io.Reader) error {
	final := filepath.Join(dir, fmt.Sprintf("%d_%s.gz", at.Unix(), name))
	f, err := os.CreateTemp(dir, ".wal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	z := gzip.NewWriter(f)
	_, err = io.Copy(z, data)
	if err == nil {
		err = z.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), final)
}

// Collect WAL from every database that archives it. Repeated failures are
// recorded once, so a stopped database does not flood Events.
func (m *manager) collectAllWAL(ctx context.Context) {
	if !m.pitr.collecting.TryLock() {
		return
	}
	defer m.pitr.collecting.Unlock()
	records, err := m.records()
	if err != nil {
		return
	}
	m.mu.Lock()
	busy := maps.Clone(m.busy)
	m.mu.Unlock()
	var states map[string]string
	for _, r := range records {
		if !r.archiving() || r.PendingRestore || busy[r.Name] { // an upgrade or restore collects for itself
			continue
		}
		if states == nil {
			if states, err = m.states(ctx); err != nil {
				return
			}
		}
		if states[r.Project] != "running" && states[r.Project] != "unhealthy" {
			continue
		}
		for {
			n, err := m.collectLogs(ctx, r)
			m.collectionResult(ctx, r, err)
			if err != nil || n < walBatch {
				break
			}
		}
	}
}

// Collect a database's closed logs: WAL for PostgreSQL, binary logs for MySQL.
func (m *manager) collectLogs(ctx context.Context, r record) (int, error) {
	if r.Engine == "mysql" {
		return m.collectBinlogs(ctx, r)
	}
	return m.collectWAL(ctx, r)
}

func (m *manager) collectionResult(ctx context.Context, r record, err error) {
	m.pitr.mu.Lock()
	if m.pitr.failures == nil {
		m.pitr.failures = make(map[string]int)
	}
	before := m.pitr.failures[r.Project]
	if err != nil {
		m.pitr.failures[r.Project]++
	} else {
		delete(m.pitr.failures, r.Project)
	}
	after := m.pitr.failures[r.Project]
	m.pitr.mu.Unlock()
	action := "Collect WAL for point-in-time recovery"
	if r.Engine == "mysql" {
		action = "Collect binary logs for point-in-time recovery"
	}
	switch {
	case after == 5:
		m.note(ctx, r.Name, action, "failed", strings.TrimPrefix(err.Error(), "docker: "))
	case before >= 5 && after == 0:
		m.note(ctx, r.Name, action, "completed", "Collecting again.")
	}
}

// ---------- the recovery window

type recoveryWindow struct {
	Earliest *time.Time `json:"earliest,omitempty"`
	Latest   *time.Time `json:"latest,omitempty"`
}

// From the end of the oldest base backup of the current data to the newest
// archived log. Empty until the first base backup exists.
func (m *manager) window(r record, backups []backup) recoveryWindow {
	var w recoveryWindow
	for _, b := range backups {
		if b.pitrBase() && b.Name == r.Name && b.DataKey == r.diskKey() && (w.Earliest == nil || b.Created.Before(*w.Earliest)) {
			t := b.Created
			w.Earliest = &t
		}
	}
	if latest := m.latestWAL(r.diskKey()); w.Earliest != nil && latest.After(*w.Earliest) {
		w.Latest = &latest
	} else if w.Earliest != nil {
		t := *w.Earliest
		w.Latest = &t
	}
	return w
}

// ---------- pruning

// Remove logs that no base backup needs any more: older than the start of
// the oldest base backup of their data, or all of them when none is left.
func (m *manager) pruneWAL(ctx context.Context, backups []backup) {
	keys, err := os.ReadDir(filepath.Join(m.dir, "pitr"))
	if err != nil {
		return
	}
	for _, k := range keys {
		if !k.IsDir() {
			continue
		}
		var oldest *time.Time
		for _, b := range backups {
			if b.pitrBase() && b.DataKey == k.Name() && (oldest == nil || b.Started.Before(*oldest)) {
				t := b.Started
				oldest = &t
			}
		}
		files, err := m.walFiles(k.Name())
		if err != nil {
			continue
		}
		var removed []walFile
		for _, f := range files {
			if oldest == nil && time.Since(f.At) < 24*time.Hour {
				continue // a database that just turned PITR on is taking its first base backup
			}
			if oldest == nil || f.At.Before(oldest.Add(-2*time.Minute)) {
				if os.Remove(filepath.Join(m.walDir(k.Name()), f.Stored)) == nil {
					removed = append(removed, f)
				}
			}
		}
		if len(removed) > 0 {
			go m.removeRemoteWAL(k.Name(), removed)
		}
	}
}

// ---------- backup storage

func uploadedPath(dir string) string { return filepath.Join(dir, ".uploaded") }

func readUploaded(dir string) map[string]bool {
	set := map[string]bool{}
	f, err := os.Open(uploadedPath(dir))
	if err != nil {
		return set
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		set[s.Text()] = true
	}
	return set
}

func walKey(cfg offsiteConfig, dataKey, stored string) string {
	return cfg.Prefix + "wal/" + dataKey + "/" + stored
}

// Copy collected WAL to backup storage, oldest first, remembering what is
// there. Runs with every backup storage sweep.
func (m *manager) syncWAL(ctx context.Context, c *s3, cfg offsiteConfig) error {
	keys, err := os.ReadDir(filepath.Join(m.dir, "pitr"))
	if err != nil {
		return nil
	}
	for _, k := range keys {
		if !k.IsDir() {
			continue
		}
		dir := m.walDir(k.Name())
		done := readUploaded(dir)
		files, err := m.walFiles(k.Name())
		if err != nil {
			return err
		}
		for _, f := range files {
			if done[f.Stored] || ctx.Err() != nil {
				continue
			}
			file, err := os.Open(filepath.Join(dir, f.Stored))
			if err != nil {
				continue // pruned meanwhile
			}
			info, _ := file.Stat()
			err = c.putFile(ctx, walKey(cfg, k.Name(), f.Stored), file, info.Size())
			file.Close()
			if err != nil {
				return fmt.Errorf("copying WAL %s: %w", f.Name, err)
			}
			list, err := os.OpenFile(uploadedPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			fmt.Fprintln(list, f.Stored)
			list.Close()
		}
	}
	return nil
}

func (m *manager) removeRemoteWAL(dataKey string, files []walFile) {
	cfg, err := m.loadOffsite()
	if err != nil || !cfg.Enabled {
		return
	}
	c, err := newS3(cfg)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.backgroundContext(), 10*time.Minute)
	defer cancel()
	dir := m.walDir(dataKey)
	done := readUploaded(dir)
	for _, f := range files {
		if done[f.Stored] {
			if err := c.delete(ctx, walKey(cfg, dataKey, f.Stored)); err != nil {
				log.Printf("deleting the storage copy of WAL %s: %v", f.Name, err)
				return
			}
			delete(done, f.Stored)
		}
	}
	var keep []string
	for name := range done {
		keep = append(keep, name)
	}
	slices.Sort(keep)
	if err := writeFile(uploadedPath(dir), []byte(strings.Join(keep, "\n")+"\n")); err != nil {
		log.Printf("recording WAL copies: %v", err)
	}
}

// ---------- restore to a time

// Every log of a data key, in the library and, for files it lacks, in backup
// storage, oldest first, with a way to read each and its stored size.
func (m *manager) storedLogs(ctx context.Context, dataKey string) ([]walFile, func(walFile) (io.ReadCloser, int64, error), error) {
	local, err := m.walFiles(dataKey)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, f := range local {
		have[f.Stored] = true
	}
	all := slices.Clone(local)
	var c *s3
	cfg, _ := m.loadOffsite()
	if cfg.Enabled {
		if c, err = newS3(cfg); err == nil {
			objects, err := c.list(ctx, cfg.Prefix+"wal/"+dataKey+"/")
			if err != nil {
				return nil, nil, fail(502, "Could not list logs in backup storage: "+err.Error())
			}
			for _, o := range objects {
				if f, ok := parseWAL(o.Key[strings.LastIndex(o.Key, "/")+1:]); ok && !have[f.Stored] {
					all = append(all, f)
				}
			}
		}
	}
	sortWAL(all)
	open := func(f walFile) (io.ReadCloser, int64, error) {
		if have[f.Stored] {
			file, err := os.Open(filepath.Join(m.walDir(dataKey), f.Stored))
			if err != nil {
				return nil, 0, err
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return nil, 0, err
			}
			return file, info.Size(), nil
		}
		if c == nil {
			return nil, 0, errors.New("connect backup storage to read " + f.Name)
		}
		return c.get(ctx, walKey(cfg, dataKey, f.Stored))
	}
	return all, open, nil
}

// The WAL a restore to target needs: from the base backup's start to a
// little past the target, plus timeline history.
func (m *manager) walForRestore(ctx context.Context, b backup, target time.Time) ([]walFile, func(walFile) (io.ReadCloser, error), error) {
	all, open, err := m.storedLogs(ctx, b.DataKey)
	if err != nil {
		return nil, nil, err
	}
	from, to := b.Started.Add(-2*time.Minute), target.Add(10*time.Minute)
	var picked []walFile
	for _, f := range all {
		if !walNamePattern.MatchString(f.Name) {
			continue
		}
		if strings.HasSuffix(f.Name, ".history") || !f.At.Before(from) && !f.At.After(to) {
			picked = append(picked, f)
		}
	}
	return picked, func(f walFile) (io.ReadCloser, error) {
		body, _, err := open(f)
		return body, err
	}, nil
}

// Stream WAL files as an uncompressed tar, for the new database to replay.
func writeWALTar(w io.Writer, files []walFile, open func(walFile) (io.ReadCloser, error)) error {
	tw := tar.NewWriter(w)
	for _, f := range files {
		body, err := open(f)
		if err != nil {
			return err
		}
		z, err := gzip.NewReader(body)
		if err != nil {
			body.Close()
			return fmt.Errorf("WAL %s is damaged: %w", f.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(z, 1<<30))
		body.Close()
		if err != nil {
			return fmt.Errorf("reading WAL %s: %w", f.Name, err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0600, Size: int64(len(data)), ModTime: f.At}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	return tw.Close()
}

const physicalBaseScript = `mkdir -p "$PGDATA" && tar -x -C "$PGDATA"`

const physicalWALScript = `mkdir -p "$PGDATA/openrds_wal" && tar -x -C "$PGDATA/openrds_wal"
cat >> "$PGDATA/postgresql.auto.conf" <<CONF
restore_command = 'cp $PGDATA/openrds_wal/%%f %%p'
recovery_target_time = '%s'
recovery_target_action = 'promote'
CONF
touch "$PGDATA/recovery.signal"`

// After a physical restore: the new owner password, source users turned
// off (their passwords belong to the source), recovery settings removed.
const physicalCleanupSQL = `ALTER SYSTEM RESET restore_command;
ALTER SYSTEM RESET recovery_target_time;
ALTER SYSTEM RESET recovery_target_action;
SELECT pg_reload_conf();
DO $$ DECLARE u record; BEGIN
  FOR u IN SELECT rolname FROM pg_roles WHERE rolcanlogin AND rolname <> 'openrds' LOOP
    EXECUTE format('ALTER ROLE %%I NOLOGIN PASSWORD NULL', u.rolname);
  END LOOP;
END $$;
ALTER ROLE openrds PASSWORD '%s';
`

// Fill a new database's empty volume from a base backup, with WAL up to the
// target when there is one, then start it: PostgreSQL replays to the target
// and opens on a new timeline.
func (m *manager) restorePhysical(ctx context.Context, r *record, b backup, target *time.Time) error {
	data, closer, err := m.openBackupData(b)
	if err != nil {
		return err
	}
	err = m.compose(ctx, *r, data, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec", physicalBaseScript)
	closer.Close()
	if err != nil {
		return fmt.Errorf("writing the base backup failed: %w", err)
	}
	if target != nil {
		files, open, err := m.walForRestore(ctx, b, *target)
		if err != nil {
			return err
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeWALTar(pw, files, open)) }()
		script := fmt.Sprintf(physicalWALScript, target.UTC().Format("2006-01-02 15:04:05.999999-07"))
		err = m.compose(ctx, *r, pr, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec", script)
		pr.Close()
		if err != nil {
			return fmt.Errorf("writing WAL failed: %w", err)
		}
	}
	if err := m.compose(ctx, *r, nil, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec",
		`chown -R postgres:postgres "$PGDATA" && chmod 700 "$PGDATA"`); err != nil {
		return err
	}
	if err := m.start(ctx, r); err != nil {
		if target == nil || !m.recoveryEndedEarly(ctx, *r) {
			return fmt.Errorf("the restored database did not start: %w", err)
		}
		// Nothing was committed between the target and the end of the WAL
		// (a quiet database), which PostgreSQL 13+ reports as an error.
		// Replaying all of that WAL gives the database as it was at the target.
		if err := m.compose(ctx, *r, nil, nil, "stop", "--timeout", stopWait, "db"); err != nil {
			return err
		}
		if err := m.compose(ctx, *r, nil, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec",
			`sed -i '/^recovery_target_time/d' "$PGDATA/postgresql.auto.conf"`); err != nil {
			return err
		}
		if err := m.start(ctx, r); err != nil {
			return fmt.Errorf("the restored database did not start: %w", err)
		}
	}
	if err := m.admin(ctx, *r, fmt.Sprintf(physicalCleanupSQL, pgPassword(r.Password))); err != nil {
		return fmt.Errorf("setting the new owner password failed: %w", err)
	}
	return m.compose(ctx, *r, nil, nil, "exec", "-T", "db", "sh", "-ec", `rm -rf "$PGDATA/openrds_wal"`)
}

// The base backup a restore to target starts from: the newest base backup
// of the database's data that ended before the target.
func (m *manager) baseFor(r record, target time.Time) (backup, recoveryWindow, error) {
	backups, err := m.backups()
	if err != nil {
		return backup{}, recoveryWindow{}, err
	}
	w := m.window(r, backups)
	if w.Earliest == nil {
		return backup{}, w, fail(409, r.Name+" has no point-in-time recovery window yet: turn it on, and the first base backup starts it.")
	}
	if target.Before(*w.Earliest) || target.After(*w.Latest) {
		return backup{}, w, fail(400, fmt.Sprintf("Choose a time from %s to %s UTC.", w.Earliest.Format("2006-01-02 15:04:05"), w.Latest.Format("2006-01-02 15:04:05")))
	}
	var base *backup
	for i := range backups { // newest first
		b := backups[i]
		if b.pitrBase() && b.Name == r.Name && b.DataKey == r.diskKey() && !b.Created.After(target) {
			base = &b
			break
		}
	}
	if base == nil {
		return backup{}, w, fail(409, "No base backup covers that time.")
	}
	return *base, w, nil
}

// Restore a database as it was at a time, into a new database with the
// source's size, storage, parameters and TLS setting.
func (m *manager) restoreToTime(ctx context.Context, name string, req restoreToTimeRequest) error {
	src, err := m.record(name)
	if err != nil {
		return err
	}
	if req.TargetTime.IsZero() {
		return fail(400, "Choose the time to restore to.")
	}
	b, _, err := m.baseFor(src, req.TargetTime)
	if err != nil {
		return err
	}
	spec := instanceSpec{Name: req.Name, Engine: src.Engine, Version: src.version(), Server: src.Server, Port: req.Port, CPUs: src.CPUs, MemoryMiB: src.MemoryMiB,
		DiskGiB: src.DiskGiB, Parameters: src.Parameters, RequireTLS: src.RequireTLS, BackupRetentionDays: src.BackupRetentionDays, BackupWindow: src.BackupWindow,
		BackupIntervalHours: src.BackupIntervalHours, Tags: src.Tags}
	target := req.TargetTime.UTC()
	return m.provisionAt(ctx, spec, &b, &target)
}

type restoreToTimeRequest struct {
	Name       string    `json:"name"`
	Port       int       `json:"port"`
	TargetTime time.Time `json:"targetTime"`
}

func pitrRoutes(add func(string, int, endpoint), m *manager) {
	add("POST /api/instances/{name}/restore-to-time", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var req restoreToTimeRequest
		if err := decode(w, r, &req); err != nil {
			return err
		}
		if err := m.restoreToTime(r.Context(), r.PathValue("name"), req); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": req.Name})
		return nil
	})
}

// Whether PostgreSQL stopped because the WAL ended before any commit after
// the target time.
func (m *manager) recoveryEndedEarly(ctx context.Context, r record) bool {
	out := tailBuffer{limit: 32 << 10}
	m.compose(ctx, r, nil, &out, "logs", "--no-color", "--tail", "60", "db")
	return strings.Contains(string(out.data), "recovery ended before configured recovery target was reached")
}
