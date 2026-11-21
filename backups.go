package main

import (
	"context"
	"errors"
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

func (m *manager) makeBackup(ctx context.Context, name string) (b backup, err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return b, err
	}
	defer release()
	if r.PendingRestore {
		return b, fail(409, "This instance has an incomplete restore.")
	}
	finish := m.track(name, "Create backup")
	defer func() { finish(err) }()
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
	b = backup{randomHex(16), name, r.Engine, time.Now().UTC(), info.Size()}
	if err := writeJSON(filepath.Join(stage, "meta.json"), b); err != nil {
		return b, err
	}
	return b, os.Rename(stage, filepath.Join(m.backupDir(), b.ID))
}

func (m *manager) deleteBackup(b backup) error {
	// Unpublish first so readers never discover a half-deleted backup.
	dir := filepath.Join(m.backupDir(), b.ID)
	dest := filepath.Join(m.backupDir(), ".deleting-"+b.ID)
	if err := os.Rename(dir, dest); err != nil {
		return err
	}
	err := os.RemoveAll(dest)
	if err == nil {
		m.note(b.Name, "Delete backup", "completed")
	}
	return err
}
