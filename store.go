package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$|^[a-z]$`)
	idPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

type problem struct {
	status int
	text   string
}

func (p *problem) Error() string { return p.text }

func fail(status int, text string) error { return &problem{status, text} }

type instanceSpec struct {
	Name      string  `json:"name"`
	Engine    string  `json:"engine"`
	Port      int     `json:"port"`
	CPUs      float64 `json:"cpus"`
	MemoryMiB int     `json:"memoryMiB"`
	DiskGiB   int     `json:"diskGiB"`
}

// Never serialize record in API responses: it contains database credentials.
type record struct {
	instanceSpec
	Project        string    `json:"project"`
	Password       string    `json:"password"`
	RootPassword   string    `json:"rootPassword,omitempty"`
	Created        time.Time `json:"createdAt"`
	PendingRestore bool      `json:"pendingRestore,omitempty"`
}

type manager struct {
	dir        string
	templates  string
	docker     dockerRunner
	mu         sync.Mutex
	busy       map[string]bool
	activityMu sync.Mutex
	quotaCache quotaCache
}

func openManager(dir, templates string, docker dockerRunner) (*manager, error) {
	for _, sub := range []string{"instances", "backups"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			return nil, err
		}
	}
	return &manager{dir: dir, templates: templates, docker: docker, busy: make(map[string]bool)}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Write in the destination directory so a reader sees either complete version.
func writeJSON(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".writing-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(value); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func (m *manager) recordPath(name string) string {
	return filepath.Join(m.dir, "instances", name+".json")
}

func (m *manager) record(name string) (record, error) {
	var r record
	if !namePattern.MatchString(name) {
		return r, fail(400, "Use a 1–40 character lowercase database name, starting with a letter and ending with a letter or digit.")
	}
	err := readJSON(m.recordPath(name), &r)
	if errors.Is(err, os.ErrNotExist) {
		return r, fail(404, "Database instance not found.")
	}
	return r, err
}

func (m *manager) records() ([]record, error) {
	entries, err := os.ReadDir(filepath.Join(m.dir, "instances"))
	if err != nil {
		return nil, err
	}
	var records []record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var r record
		err := readJSON(filepath.Join(m.dir, "instances", entry.Name()), &r)
		if errors.Is(err, os.ErrNotExist) {
			continue // deleted during listing
		}
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

// Reserve a name for one operation; reserve a port and persist credentials
// before asking Docker to create anything. Independent instances can proceed.
func (m *manager) begin(name string, create *instanceSpec, restoring bool) (record, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var r record
	if !namePattern.MatchString(name) {
		return r, nil, fail(400, "Use a 1–40 character lowercase database name, starting with a letter and ending with a letter or digit.")
	}
	if m.busy[name] {
		return r, nil, fail(409, "An operation is already running for this instance.")
	}
	if create != nil {
		if _, ok := engines[create.Engine]; !ok {
			return r, nil, fail(400, "Choose postgres, mysql, or valkey.")
		}
		if create.Port < 1024 || create.Port > 65535 {
			return r, nil, fail(400, "Choose a host port between 1024 and 65535.")
		}
		if create.CPUs != 0 && (create.CPUs < 0.1 || create.CPUs > 1024) {
			return r, nil, fail(400, "The CPU limit must be at least 0.1, or zero for unlimited.")
		}
		minMemory := 128
		if create.Engine == "mysql" {
			minMemory = 512
		}
		if create.MemoryMiB != 0 && (create.MemoryMiB < minMemory || create.MemoryMiB > 16777216) {
			return r, nil, fail(400, fmt.Sprintf("The memory limit must be at least %d MiB, or zero for unlimited.", minMemory))
		}
		all, err := m.records()
		if err != nil {
			return r, nil, err
		}
		for _, other := range all {
			if other.Name == name {
				return r, nil, fail(409, "That database name is already in use.")
			}
			if other.Port == create.Port {
				return r, nil, fail(409, fmt.Sprintf("Port %d is reserved by %s.", other.Port, other.Name))
			}
		}
		r = record{instanceSpec: *create, Project: "openrds-db-" + randomHex(16),
			Password: randomHex(24), Created: time.Now().UTC(), PendingRestore: restoring}
		if r.Engine == "mysql" {
			r.RootPassword = randomHex(24)
		}
		if err := writeJSON(m.recordPath(name), r); err != nil {
			return r, nil, err
		}
	} else {
		var err error
		if r, err = m.record(name); err != nil {
			return r, nil, err
		}
	}
	m.busy[name] = true
	return r, func() {
		m.mu.Lock()
		delete(m.busy, name)
		m.mu.Unlock()
	}, nil
}
