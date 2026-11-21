package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type backup struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Engine  string    `json:"engine"`
	Created time.Time `json:"createdAt"`
	Bytes   int64     `json:"bytes"`
	DiskGiB int       `json:"diskGiB,omitempty"`
	// Manual snapshots stay until deleted; automated backups expire.
	Kind    string     `json:"kind"`
	Expires *time.Time `json:"expiresAt,omitempty"`
	Final   bool       `json:"final,omitempty"`
	// The source configuration, offered again when restoring.
	CPUs       float64           `json:"cpus,omitempty"`
	MemoryMiB  int               `json:"memoryMiB,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
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
	return b, err
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
	return m.snapshot(ctx, r, kind, false)
}

// Write one logical backup while the caller holds the instance.
func (m *manager) snapshot(ctx context.Context, r record, kind string, final bool) (b backup, err error) {
	stage, err := os.MkdirTemp(m.backupDir(), ".creating-")
	if err != nil {
		return b, err
	}
	defer os.RemoveAll(stage)
	f, err := os.OpenFile(filepath.Join(stage, "data"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return b, err
	}
	args := append([]string{"exec", "-T", "db"}, engines[r.Engine].dump...)
	err = m.compose(ctx, r, nil, f, args...)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
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
	if info.Size() == 0 {
		return b, fail(502, "The database returned an empty backup.")
	}
	b = backup{ID: randomHex(16), Name: r.Name, Engine: r.Engine, Created: time.Now().UTC(), Bytes: info.Size(), DiskGiB: r.DiskGiB,
		Kind: kind, Final: final, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, Parameters: maps.Clone(r.Parameters)}
	if kind == "automated" {
		expires := b.Created.AddDate(0, 0, r.BackupRetentionDays)
		b.Expires = &expires
	}
	if err := writeJSON(filepath.Join(stage, "meta.json"), b); err != nil {
		return b, err
	}
	return b, os.Rename(stage, filepath.Join(m.backupDir(), b.ID))
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
