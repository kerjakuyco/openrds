package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type backup struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Engine  string    `json:"engine"`
	Version string    `json:"version,omitempty"` // of the source database
	Created time.Time `json:"createdAt"`
	Bytes   int64     `json:"bytes"`
	// "gzip" when OpenRDS compressed the engine's dump; empty when the data
	// is the engine's own format.
	Compression string `json:"compression,omitempty"`
	DiskGiB     int    `json:"diskGiB,omitempty"`
	// Manual snapshots stay until deleted; automated backups expire.
	Kind    string     `json:"kind"`
	Expires *time.Time `json:"expiresAt,omitempty"`
	Final   bool       `json:"final,omitempty"`
	Note    string     `json:"note,omitempty"` // why OpenRDS took it, such as before an upgrade
	// "physical" for a PostgreSQL base backup, the start of point-in-time
	// recovery; empty for a logical dump.
	Format  string    `json:"format,omitempty"`
	DataKey string    `json:"dataKey,omitempty"` // whose WAL or binary logs it replays
	Started time.Time `json:"startedAt,omitzero"`
	// A MySQL base backup: the binary log position it was taken at.
	BinlogFile string `json:"binlogFile,omitempty"`
	BinlogPos  int64  `json:"binlogPos,omitempty"`
	// The source configuration, offered again when restoring.
	CPUs       float64           `json:"cpus,omitempty"`
	MemoryMiB  int               `json:"memoryMiB,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	// The copy in backup storage, when storage is connected.
	Offsite *offsiteCopy `json:"offsite,omitempty"`
}

func (m *manager) backupDir() string { return filepath.Join(m.dir, "backups") }

func (m *manager) backup(id string) (backup, error) {
	var b backup
	if !idPattern.MatchString(id) {
		return b, fail(404, "Backup not found.")
	}
	err := readJSON(filepath.Join(m.backupDir(), id, "meta.json"), &b)
	if errors.Is(err, os.ErrNotExist) {
		return b, fail(404, "Backup not found.")
	}
	if b.Kind == "" {
		b.Kind = "manual" // written before automated backups existed
	}
	b.Version = b.version() // backups from before versions existed name the version they came from
	return b, err
}

// The engine's dump of a backup, decompressed when OpenRDS compressed it.
func (m *manager) openBackupData(b backup) (io.Reader, io.Closer, error) {
	f, err := m.openDump(b.ID)
	if err != nil {
		return nil, nil, err
	}
	switch b.Compression {
	case "":
		return f, f, nil
	case "gzip":
		z, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, fail(502, "The backup data is damaged: "+err.Error())
		}
		return z, f, nil
	}
	f.Close()
	return nil, nil, fail(400, fmt.Sprintf("This backup is compressed with %q, which this OpenRDS version cannot read. Update OpenRDS, then restore it.", b.Compression))
}

// The file name a download gets, such as orders-20260101-030000.sql.gz.
func (b backup) fileName() string {
	if b.Format == "physical" {
		return b.Name + "-" + b.Created.Format("20060102-150405") + "-base.tar.gz"
	}
	name := b.Name + "-" + b.Created.Format("20060102-150405") + engines[b.Engine].suffix
	if b.Compression == "gzip" {
		name += ".gz"
	}
	return name
}

// Free space backups leave on the disk that holds them: 5% of it, from
// 2 GiB to 10 GiB. The databases usually share that disk, and a full disk
// stops every one of them.
func backupReserve(total uint64) uint64 { return min(max(2<<30, total/20), 10<<30) }

func sizeLabel(n uint64) string {
	units := []string{"bytes", "KiB", "MiB", "GiB", "TiB", "PiB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// Refuse a backup or import whose expected size would leave less than the
// reserve free. An unknown size (0) only checks the reserve itself.
func (m *manager) checkBackupSpace(expected int64) error {
	free, total, ok := m.space(m.backupDir())
	if !ok {
		return nil
	}
	reserve := backupReserve(total)
	if free >= reserve+uint64(max(expected, 0)) {
		return nil
	}
	need := ""
	if expected > 0 {
		need = ", and this backup needs about " + sizeLabel(uint64(expected))
	}
	return fail(507, fmt.Sprintf("Not enough free disk space for a backup: %s free%s. OpenRDS keeps %s free so databases keep running. Delete old snapshots or add disk space.",
		sizeLabel(free), need, sizeLabel(reserve)))
}

// Stops a backup before it fills the disk, checking free space every
// guardEvery bytes.
type spaceGuard struct {
	w       io.Writer
	dir     string
	space   func(string) (free, total uint64, ok bool)
	reserve uint64
	pending int
	err     error
}

func (m *manager) guardSpace(w io.Writer) *spaceGuard {
	g := &spaceGuard{w: w, dir: m.backupDir(), space: m.space}
	if _, total, ok := g.space(g.dir); ok {
		g.reserve = backupReserve(total)
	}
	return g
}

func (g *spaceGuard) Write(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	if g.pending += len(p); g.pending >= guardEvery {
		g.pending = 0
		if free, _, ok := g.space(g.dir); ok && free < g.reserve+uint64(len(p)) {
			g.err = fail(507, fmt.Sprintf("The backup stopped because free disk space fell to %s. OpenRDS keeps %s free so databases keep running. Delete old snapshots or add disk space.",
				sizeLabel(free), sizeLabel(g.reserve)))
			return 0, g.err
		}
	}
	return g.w.Write(p)
}

var guardEvery = 64 << 20

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// The size of the newest backup of a database, as a guess for the next one.
func (m *manager) expectedBackupBytes(name string) int64 {
	list, err := m.backups()
	if err != nil {
		return 0
	}
	for _, b := range list { // newest first
		if b.Name == name {
			return b.Bytes
		}
	}
	return 0
}

func (m *manager) openDump(id string) (*os.File, error) {
	if !idPattern.MatchString(id) {
		return nil, fail(404, "Backup not found.")
	}
	f, err := os.Open(filepath.Join(m.backupDir(), id, "data"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fail(404, "Backup data not found.")
	}
	return f, err
}

func (m *manager) backups() ([]backup, error) {
	entries, err := os.ReadDir(m.backupDir())
	if err != nil {
		return nil, err
	}
	result := []backup{}
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		b, err := m.backup(e.Name())
		var p *problem
		if errors.As(err, &p) && p.status == 404 {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Created.After(result[j].Created) })
	return result, nil
}

func (m *manager) makeBackup(ctx context.Context, name, kind string) (b backup, err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return b, err
	}
	defer release()
	if r.PendingRestore {
		return b, fail(409, "This instance has an incomplete restore.")
	}
	label := "Create snapshot"
	if kind == "automated" {
		label = "Automated backup"
	}
	finish := m.track(ctx, name, label)
	defer func() { finish(err) }()
	// With point-in-time recovery on, automated backups are base backups.
	return m.snapshot(ctx, r, kind, false, "", kind == "automated" && r.archiving())
}

// Write one backup while the caller holds the instance: a base backup for
// point-in-time recovery when base is set.
func (m *manager) snapshot(ctx context.Context, r record, kind string, final bool, note string, base bool) (b backup, err error) {
	if err := m.checkBackupSpace(m.expectedBackupBytes(r.Name)); err != nil {
		return b, err
	}
	stage, err := os.MkdirTemp(m.backupDir(), ".creating-")
	if err != nil {
		return b, err
	}
	defer os.RemoveAll(stage)
	f, err := os.OpenFile(filepath.Join(stage, "data"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return b, err
	}
	eng := engines[r.Engine]
	physical, positioned := base && r.Engine == "postgres", base && r.Engine == "mysql"
	args := append([]string{"exec", "-T", "db"}, eng.dump...)
	switch {
	case physical:
		args = append([]string{"exec", "-T", "db"}, baseBackupCommand...)
	case positioned:
		args = []string{"exec", "-T", "db", "sh", "-ec", mysqlBaseDump}
	}
	var input io.Reader
	if eng.dumpAuth {
		input = strings.NewReader(r.Password + "\n")
	}
	started := time.Now().UTC()
	guard := m.guardSpace(f)
	out := &countingWriter{w: guard}
	var z *gzip.Writer
	if eng.compress || physical {
		z, _ = gzip.NewWriterLevel(guard, gzip.BestSpeed) // fast enough to keep up with the dump
		out.w = z
	}
	var dst io.Writer = out
	head := &dumpHead{w: out}
	if positioned {
		dst = head
	}
	err = m.compose(ctx, r, input, dst, args...)
	if err == nil && z != nil {
		err = z.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if guard.err != nil {
		return b, guard.err // clearer than the broken pipe it caused
	}
	if err != nil {
		return b, fail(502, "Backup did not finish: "+err.Error())
	}
	if closeErr != nil {
		return b, closeErr
	}
	info, err := os.Stat(f.Name())
	if err != nil {
		return b, err
	}
	if out.n == 0 {
		return b, fail(502, "The database returned an empty backup.")
	}
	b = backup{ID: randomHex(16), Name: r.Name, Engine: r.Engine, Version: r.version(), Created: time.Now().UTC(), Bytes: info.Size(), DiskGiB: r.DiskGiB,
		Kind: kind, Final: final, Note: note, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, Parameters: maps.Clone(r.Parameters)}
	if z != nil {
		b.Compression = "gzip"
	}
	if physical {
		b.Format, b.DataKey, b.Started = "physical", r.diskKey(), started
	}
	if positioned {
		file, pos, ok := head.position()
		if !ok {
			return b, fail(502, "The backup did not record its binary log position.")
		}
		b.DataKey, b.Started, b.BinlogFile, b.BinlogPos = r.diskKey(), started, file, pos
	}
	if kind == "automated" {
		expires := b.Created.AddDate(0, 0, r.BackupRetentionDays)
		b.Expires = &expires
	}
	if err := writeJSON(filepath.Join(stage, "meta.json"), b); err != nil {
		return b, err
	}
	if err := os.Rename(stage, filepath.Join(m.backupDir(), b.ID)); err != nil {
		return b, err
	}
	go m.syncOffsite() // copy it to backup storage, when connected
	return b, nil
}

// Unpublish first so readers never discover a half-deleted backup.
func (m *manager) removeBackup(ctx context.Context, b backup, action string) error {
	dir := filepath.Join(m.backupDir(), b.ID)
	dest := filepath.Join(m.backupDir(), ".deleting-"+b.ID)
	if err := os.Rename(dir, dest); err != nil {
		return err
	}
	err := os.RemoveAll(dest)
	if err == nil {
		m.note(ctx, b.Name, action, "completed", "")
		go m.removeOffsite(b)
	}
	return err
}

func (m *manager) deleteBackup(ctx context.Context, id string) error {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	b, err := m.backup(id)
	if err != nil {
		return err
	}
	return m.removeBackup(ctx, b, "Delete backup")
}

// Re-read under the lock: the backup may have been kept as a snapshot meanwhile.
func (m *manager) expire(ctx context.Context, id string, now time.Time) error {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	b, err := m.backup(id)
	if err != nil || b.Kind != "automated" || b.Expires == nil || !now.After(*b.Expires) {
		return nil
	}
	return m.removeBackup(ctx, b, "Expire automated backup")
}

// Turn an automated backup into a manual snapshot that never expires.
func (m *manager) keepBackup(ctx context.Context, id string) (backup, error) {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	b, err := m.backup(id)
	if err != nil {
		return b, err
	}
	if b.Kind != "automated" {
		return b, fail(409, "This backup is already a manual snapshot.")
	}
	b.Kind, b.Expires = "manual", nil
	if err := writeJSON(filepath.Join(m.backupDir(), b.ID, "meta.json"), b); err != nil {
		return b, err
	}
	m.note(ctx, b.Name, "Keep backup as snapshot", "completed", "")
	return b, nil
}

// A changed retention period applies to the instance's existing automated backups.
func (m *manager) retain(r record) error {
	if r.BackupRetentionDays == 0 {
		return nil // turning automated backups off keeps the existing ones until they expire
	}
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	list, err := m.backups()
	if err != nil {
		return err
	}
	for _, b := range list {
		if b.Kind != "automated" || b.Name != r.Name || b.Created.Before(r.Created) {
			continue
		}
		expires := b.Created.AddDate(0, 0, r.BackupRetentionDays)
		if b.Expires != nil && b.Expires.Equal(expires) {
			continue
		}
		b.Expires = &expires
		if err := writeJSON(filepath.Join(m.backupDir(), b.ID, "meta.json"), b); err != nil {
			return err
		}
	}
	return nil
}
