package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$|^[a-z]$`)
	idPattern       = regexp.MustCompile(`^[a-f0-9]{32}$`)
	tagKeyPattern   = regexp.MustCompile(`^[\p{L}\p{N} _.:/=+\-@]{1,128}$`)
	tagValuePattern = regexp.MustCompile(`^[\p{L}\p{N} _.:/=+\-@]{0,256}$`)
)

const maxTags = 50

type problem struct {
	status int
	text   string
}

func (p *problem) Error() string { return p.text }

func fail(status int, text string) error { return &problem{status, text} }

// Returned while another operation holds the instance, so background work can retry.
var errBusy = &problem{409, "An operation is already running for this instance."}

type instanceSpec struct {
	Name                string            `json:"name"`
	Engine              string            `json:"engine"`
	Version             string            `json:"version,omitempty"` // empty: created before versions existed
	Port                int               `json:"port"`
	CPUs                float64           `json:"cpus"`
	MemoryMiB           int               `json:"memoryMiB"`
	DiskGiB             int               `json:"diskGiB"`
	BackupRetentionDays int               `json:"backupRetentionDays"`
	BackupWindow        string            `json:"backupWindow"`
	BackupIntervalHours int               `json:"backupIntervalHours,omitempty"` // 0 or 24: daily
	DeletionProtection  bool              `json:"deletionProtection"`
	Tags                map[string]string `json:"tags,omitempty"`
	Parameters          map[string]string `json:"parameters,omitempty"`
	RequireTLS          bool              `json:"requireTLS"`
	PublicAccess        bool              `json:"publicAccess"`
	PublicPort          int               `json:"publicPort,omitempty"`
	PublicHost          string            `json:"publicHost,omitempty"`
	Firewall            []firewallRule    `json:"firewall,omitempty"`
	Pooler              *poolerSpec       `json:"pooler,omitempty"` // PostgreSQL connection pooling
	PITR                bool              `json:"pitr,omitempty"`   // PostgreSQL point-in-time recovery
}

// The container settings that change only when Compose recreates the database.
type runtimeConfig struct {
	CPUs       float64           `json:"cpus"`
	MemoryMiB  int               `json:"memoryMiB"`
	Parameters map[string]string `json:"parameters,omitempty"`
	RequireTLS bool              `json:"requireTLS,omitempty"`
	PublicHost string            `json:"publicHost,omitempty"` // named in the TLS certificate
	PITR       bool              `json:"pitr,omitempty"`       // WAL archiving starts with the server
}

func (s instanceSpec) runtime() runtimeConfig {
	return runtimeConfig{s.CPUs, s.MemoryMiB, s.Parameters, s.RequireTLS, s.PublicHost, s.PITR}
}

func (c runtimeConfig) equal(o runtimeConfig) bool {
	return c.CPUs == o.CPUs && c.MemoryMiB == o.MemoryMiB && maps.Equal(c.Parameters, o.Parameters) &&
		c.RequireTLS == o.RequireTLS && c.PublicHost == o.PublicHost && c.PITR == o.PITR
}

// Containers started by this version have TLS certificates, the managed
// pg_hba.conf, Valkey users from a file and health checks that do not depend
// on the rotating password. Older containers get them at their next restart.
const currentLayout = 1

// Never serialize record in API responses: it contains database credentials.
type record struct {
	instanceSpec
	Project        string    `json:"project"`
	Password       string    `json:"password"`
	RootPassword   string    `json:"rootPassword,omitempty"`
	Created        time.Time `json:"createdAt"`
	PendingRestore bool      `json:"pendingRestore,omitempty"`
	// While a running database waits for a restart to apply changes, the
	// settings its container actually uses.
	Applied *runtimeConfig `json:"applied,omitempty"`
	Users   []dbUser       `json:"users,omitempty"`
	Layout  int            `json:"layout,omitempty"` // of the running container; see currentLayout
	// Which data volume holds the data: an engine upgrade moves it to a new one.
	DataGeneration int `json:"dataGeneration,omitempty"`
	// While an upgrade restores data, the new container has no host port and
	// another network name, so clients cannot reach a half-filled database.
	staging bool
	// Read replicas: the source a replica follows, and the password replicas
	// connect with (a source's own; a replica sees its source's).
	ReplicaOf           string `json:"replicaOf,omitempty"`
	ReplicationPassword string `json:"replicationPassword,omitempty"`
	ServerID            int    `json:"serverId,omitempty"` // MySQL replicas, and databases promoted from one
	sourceTLS           bool   // a Valkey replica connects with TLS when its source requires it
}

type manager struct {
	dir         string
	templates   string
	docker      dockerRunner
	engine      statsReader
	mu          sync.Mutex
	busy        map[string]bool
	attempts    map[string]time.Time // last automated-backup window tried, by project
	activityMu  sync.Mutex
	backupMu    sync.Mutex
	quotaCache  quotaCache
	history     history
	offsite     offsiteState
	tlsMu       sync.Mutex
	ca          *authority
	gateway     string // image for public-access gateways, once found
	space       func(path string) (free, total uint64, ok bool)
	alerts      alertState
	pitr        pitrState
	replication replicationState
}

func openManager(dir, templates string, docker dockerRunner) (*manager, error) {
	for _, sub := range []string{"instances", "backups", "compose", "tls"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			return nil, err
		}
	}
	m := &manager{dir: dir, templates: templates, docker: docker, busy: make(map[string]bool), attempts: make(map[string]time.Time), space: diskSpace}
	m.history.init(filepath.Join(dir, "metrics.json"), defaultSampleInterval)
	return m, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Write in the destination directory so a reader sees either complete version.
func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".writing-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
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

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
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
	if err == nil && r.ReplicaOf != "" {
		var src record
		if readJSON(m.recordPath(r.ReplicaOf), &src) == nil {
			r.follow(src)
		}
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
	for i, r := range records {
		if r.ReplicaOf == "" {
			continue
		}
		for _, src := range records {
			if src.Name == r.ReplicaOf {
				records[i].follow(src)
			}
		}
	}
	return records, nil
}

// Validate everything about a configuration that does not depend on other instances.
func checkSpec(s instanceSpec) error {
	eng, ok := engines[s.Engine]
	if !ok {
		return fail(400, "Choose postgres, mysql, or valkey.")
	}
	if s.Port < 1024 || s.Port > 65535 {
		return fail(400, "Choose a host port between 1024 and 65535.")
	}
	if s.CPUs != 0 && (s.CPUs < 0.1 || s.CPUs > 1024) {
		return fail(400, "The CPU limit must be at least 0.1, or zero for unlimited.")
	}
	if s.MemoryMiB != 0 && (s.MemoryMiB < eng.minMemory || s.MemoryMiB > 16777216) {
		return fail(400, fmt.Sprintf("The memory limit must be at least %d MiB, or zero for unlimited.", eng.minMemory))
	}
	if s.BackupRetentionDays < 0 || s.BackupRetentionDays > 35 {
		return fail(400, "Keep automated backups for 1–35 days, or choose zero to turn them off.")
	}
	if s.BackupIntervalHours != 0 && !slices.Contains(backupIntervals, s.BackupIntervalHours) {
		return fail(400, "Back up every 1, 2, 3, 4, 6, 12 or 24 hours.")
	}
	if _, _, ok := parseWindow(s.BackupWindow); s.BackupWindow != "" && !ok {
		return fail(400, "Use a UTC backup window such as 03:00-03:30 that lasts at least 30 minutes.")
	}
	if len(s.Tags) > maxTags {
		return fail(400, fmt.Sprintf("Use at most %d tags.", maxTags))
	}
	for key, value := range s.Tags {
		if !tagKeyPattern.MatchString(key) || strings.TrimSpace(key) != key || !tagValuePattern.MatchString(value) {
			return fail(400, "Tag keys have 1–128 characters and values up to 256: letters, digits, spaces and _ . : / = + - @.")
		}
	}
	if err := checkNetwork(s); err != nil {
		return err
	}
	if err := checkPooler(s); err != nil {
		return err
	}
	if err := checkPITR(s); err != nil {
		return err
	}
	return checkParameters(s.Engine, s.MemoryMiB, s.Parameters)
}

// The host ports an instance holds: its loopback port, and its public port
// while public access is on.
func (s instanceSpec) hostPorts() []int {
	ports := []int{s.Port}
	if s.PublicAccess {
		ports = append(ports, s.PublicPort)
	}
	if s.Pooler.on() {
		ports = append(ports, s.Pooler.Port)
	}
	return ports
}

// Refuse host ports held by another instance or by the console itself.
func checkPorts(all []record, s instanceSpec) error {
	for _, port := range s.hostPorts() {
		if port == consolePort() {
			return fail(409, fmt.Sprintf("Port %d is used by the OpenRDS console.", port))
		}
		for _, other := range all {
			if other.Name != s.Name && slices.Contains(other.hostPorts(), port) {
				return fail(409, fmt.Sprintf("Port %d is reserved by %s.", port, other.Name))
			}
		}
	}
	return nil
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
		return r, nil, errBusy
	}
	if create != nil {
		if err := checkSpec(*create); err != nil {
			return r, nil, err
		}
		if create.BackupWindow == "" {
			create.BackupWindow = defaultBackupWindow
		}
		if create.BackupIntervalHours == 24 {
			create.BackupIntervalHours = 0 // daily, stored as records from before intervals
		}
		all, err := m.records()
		if err != nil {
			return r, nil, err
		}
		for _, other := range all {
			if other.Name == name {
				return r, nil, fail(409, "That database name is already in use.")
			}
		}
		create.Name = name
		if err := checkPorts(all, *create); err != nil {
			return r, nil, err
		}
		if create.Version == "" {
			create.Version = defaultVersion(create.Engine)
		}
		if !offeredVersion(create.Engine, create.Version) {
			return r, nil, fail(400, fmt.Sprintf("Choose a listed %s version.", engines[create.Engine].title))
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

// Persist changed settings. Holding the reservation lock keeps a port change
// atomic with provisioning and other modifications.
func (m *manager) save(r record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.records()
	if err != nil {
		return err
	}
	if err := checkPorts(all, r.instanceSpec); err != nil {
		return err
	}
	return writeJSON(m.recordPath(r.Name), r)
}
