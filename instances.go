package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

type instanceView struct {
	instanceSpec
	EngineLabel    string             `json:"engineLabel"`
	Status         string             `json:"status"`
	Busy           bool               `json:"busy"`
	Created        time.Time          `json:"createdAt"`
	PendingRestore bool               `json:"pendingRestore"`
	User           string             `json:"user"`
	Database       string             `json:"database"`
	Applied        *runtimeConfig     `json:"applied,omitempty"` // in use until a restart applies pending changes
	Tuned          map[string]string  `json:"tuned,omitempty"`   // memory settings sized to the memory limit
	Users          []dbUserView       `json:"users"`
	TLSReady       bool               `json:"tlsReady"` // the container has certificates and managed credentials
	LatestBackup   *time.Time         `json:"latestBackupAt,omitempty"`
	NextBackup     *time.Time         `json:"nextBackupAt,omitempty"`
	Live           *point             `json:"live,omitempty"`
	PoolerStatus   string             `json:"poolerStatus,omitempty"`
	Recovery       *recoveryWindow    `json:"recovery,omitempty"` // point-in-time recovery window
	ReplicaOf      string             `json:"replicaOf,omitempty"`
	Replicas       []string           `json:"replicas,omitempty"`
	Replication    *replicationStatus `json:"replication,omitempty"` // a replica's, as last read
}

func (m *manager) instances(ctx context.Context) ([]instanceView, error) {
	all, err := m.records()
	if err != nil {
		return nil, err
	}
	states, err := m.states(ctx)
	if err != nil {
		return nil, fail(503, "Cannot read database status: "+err.Error())
	}
	backups, err := m.backups()
	if err != nil {
		return nil, err
	}
	automated := latestAutomated(all, backups)
	var poolers map[string]string
	if slices.ContainsFunc(all, func(r record) bool { return r.Pooler.on() }) {
		if poolers, err = m.statesFor(ctx, "io.openrds.pooler=true"); err != nil {
			return nil, fail(503, "Cannot read connection pooler status: "+err.Error())
		}
	}
	latest := make(map[string]time.Time)
	for _, r := range all {
		for _, b := range backups {
			if b.Name == r.Name && !b.Created.Before(r.Created) && b.Created.After(latest[r.Name]) {
				latest[r.Name] = b.Created
			}
		}
	}
	now := time.Now().UTC()
	result := make([]instanceView, 0, len(all))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range all {
		state := states[r.Project]
		if state == "" {
			state = "not provisioned"
		}
		eng := engines[r.Engine]
		v := instanceView{instanceSpec: r.instanceSpec, EngineLabel: r.engineLabel(), Status: state, Busy: m.busy[r.Name],
			Created: r.Created, PendingRestore: r.PendingRestore, User: eng.user, Database: eng.database, Applied: r.Applied,
			Users: r.userViews(), TLSReady: r.Layout >= currentLayout, Tuned: tunedParameters(r.Engine, r.MemoryMiB)}
		if v.BackupWindow == "" {
			v.BackupWindow = defaultBackupWindow
		}
		v.BackupIntervalHours = int(backupInterval(r.BackupIntervalHours) / time.Hour)
		v.Version = r.version()
		if t, ok := latest[r.Name]; ok {
			v.LatestBackup = &t
		}
		if r.BackupRetentionDays > 0 {
			next := nextBackup(v.BackupWindow, r.BackupIntervalHours, automated[r.Name], now)
			v.NextBackup = &next
		}
		if state == "running" {
			v.Live = m.history.latest(r.Project, now)
		}
		if r.PITR || r.archiving() {
			w := m.window(r, backups)
			v.Recovery = &w
		}
		if r.Pooler.on() {
			v.PoolerStatus = poolers[r.Project]
			if v.PoolerStatus == "" {
				v.PoolerStatus = "not running"
			}
		}
		for _, rep := range replicasOf(all, r.Name) {
			v.Replicas = append(v.Replicas, rep.Name)
		}
		if r.ReplicaOf != "" {
			v.ReplicaOf = r.ReplicaOf
			st, ok := m.cachedReplication(r.Name)
			switch {
			case r.PendingRestore:
				st, ok = replicationStatus{State: "broken", Detail: "The replica was never completed. Delete it and try again."}, true
			case state != "running":
				st, ok = replicationStatus{State: "stopped"}, true
			}
			if ok {
				v.Replication = &st
			}
		}
		result = append(result, v)
	}
	return result, nil
}

// Retain metadata (and any provisioned volume) on Docker failure so the
// operator can retry Start or explicitly delete the instance.
func (m *manager) provision(ctx context.Context, spec instanceSpec, from *backup) error {
	return m.provisionAt(ctx, spec, from, nil)
}

// Like provision, rolling a physical backup forward to target when given.
func (m *manager) provisionAt(ctx context.Context, spec instanceSpec, from *backup, target *time.Time) (err error) {
	if err := m.checkDiskRequest(ctx, spec.DiskGiB); err != nil {
		return err // reject before reserving a name/port or touching an instance
	}
	physical := from != nil && from.Format == "physical"
	if target != nil && !physical {
		return fail(400, "Only physical backups, taken with point-in-time recovery on, restore to a time.")
	}
	var input io.Reader
	if physical {
		spec.Engine = from.Engine
		if spec.Version == "" {
			spec.Version = from.version()
		}
		if spec.Version != from.version() {
			return fail(400, fmt.Sprintf("A physical backup restores into %s %s, the version it comes from.", engines[from.Engine].title, from.version()))
		}
	} else if from != nil {
		spec.Engine = from.Engine
		if spec.Version == "" {
			spec.Version = from.version()
		}
		// Newer versions read older backups, but not the other way round.
		if compareVersions(spec.Version, from.version()) < 0 {
			return fail(400, fmt.Sprintf("Restore into %s %s or newer: the backup comes from that version.", engines[from.Engine].title, from.version()))
		}
		var data io.Closer
		input, data, err = m.openBackupData(*from)
		if err != nil {
			return err
		}
		defer data.Close()
	}
	r, release, err := m.begin(spec.Name, &spec, from != nil)
	if err != nil {
		return err
	}
	defer release()
	action, detail := "Provision database", ""
	switch {
	case target != nil:
		action, detail = "Restore to point in time", fmt.Sprintf("%s as of %s UTC", from.Name, target.Format("2006-01-02 15:04:05"))
	case from != nil:
		action = "Restore backup"
	}
	finish := m.trackDetail(ctx, r.Name, action, detail)
	defer func() { finish(err) }()
	if physical {
		if r.DiskGiB > 0 {
			if err := m.attachDisk(ctx, r); err != nil {
				return fail(502, "Restore did not finish: the disk could not be prepared. "+err.Error())
			}
		}
		if err := m.restorePhysical(ctx, &r, *from, target); err != nil {
			return fail(502, "Restore did not finish. The new instance is marked incomplete; the backup is intact. "+err.Error())
		}
		r.PendingRestore = false
		return writeJSON(m.recordPath(r.Name), r)
	}
	if err := m.start(ctx, &r); err != nil {
		return fail(502, "Provisioning did not finish. The instance was kept for inspection. "+err.Error())
	}
	if from != nil {
		if err := m.restoreInto(ctx, r, input); err != nil {
			return fail(502, "Restore did not finish. The new instance is marked incomplete; the backup is intact. "+err.Error())
		}
		r.PendingRestore = false
		return writeJSON(m.recordPath(r.Name), r)
	}
	return nil
}

func (m *manager) restoreInto(ctx context.Context, r record, input io.Reader) error {
	eng := engines[r.Engine]
	if !eng.restoreStopped {
		args := append([]string{"exec", "-T", "db"}, eng.restore...)
		return m.compose(ctx, r, input, nil, args...)
	}
	if err := m.compose(ctx, r, nil, nil, "stop", "--timeout", stopWait, "db"); err != nil {
		return err
	}
	script := eng.restore[len(eng.restore)-1]
	if err := m.compose(ctx, r, input, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec", script); err != nil {
		return err
	}
	return m.up(ctx, r)
}

func (m *manager) resizeDisk(ctx context.Context, name string, gib int) (err error) {
	if gib == 0 {
		return fail(400, "A disk limit cannot be removed in place. Restore a backup into an uncapped instance instead.")
	}
	if err := m.checkDiskRequest(ctx, gib); err != nil {
		return err
	}
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	if r.DiskGiB == 0 {
		return fail(409, "This instance has no disk limit. Restore a backup into a new instance to add one.")
	}
	if r.PendingRestore {
		return fail(409, "This instance has an incomplete restore.")
	}
	if r.DiskGiB == gib {
		return nil
	}
	states, err := m.states(ctx)
	if err != nil {
		return fail(503, "Cannot read database status: "+err.Error())
	}
	if state := states[r.Project]; state != "" && state != "stopped" {
		return fail(409, "Stop the database before changing its disk limit.")
	}
	finish := m.track(ctx, name, "Resize disk")
	defer func() { finish(err) }()
	if err = m.diskHelper(ctx, "resize", r, gib); err != nil {
		return fail(502, "Disk resize did not finish. "+err.Error())
	}
	r.DiskGiB = gib
	return writeJSON(m.recordPath(name), r)
}

// How long a database may take to become healthy, for example while it
// replays its write-ahead log after the host lost power. The start_period of
// the health checks in engines/ fits inside it. The long start period does
// not delay a crash: Docker marks a container unhealthy when it exits, so
// Compose reports a database that refuses a setting within seconds.
const startWait = "900"

// Seconds a database gets to finish writes and flush its caches when it
// stops. The templates' stop_grace_period matches, so Docker waits as long
// when the host shuts down.
const stopWait = "120"

// Create or start the database container and wait until it is healthy.
func (m *manager) up(ctx context.Context, r record) error {
	return m.compose(ctx, r, nil, nil, "up", "--detach", "--wait", "--wait-timeout", startWait, "db")
}

// Create or start the container from the stored configuration. Compose
// recreates it when settings changed, which applies pending modifications.
// The record is updated to what now runs.
func (m *manager) start(ctx context.Context, r *record) error {
	if r.DiskGiB > 0 {
		if err := m.checkDiskRequest(ctx, r.DiskGiB); err != nil {
			return err
		}
		if err := m.checkExistingVolume(ctx, *r); err != nil {
			return err
		}
		if err := m.attachDisk(ctx, *r); err != nil {
			return err
		}
	}
	if r.ReplicaOf != "" {
		// Join the source's network before the server starts, so it connects at once.
		if err := m.compose(ctx, *r, nil, nil, "up", "--no-start", "db"); err != nil {
			return err
		}
		if err := m.linkReplica(ctx, *r); err != nil {
			return err
		}
	}
	if err := m.up(ctx, *r); err != nil {
		return err
	}
	if r.ReplicaOf == "" && r.ReplicationPassword != "" {
		switch r.Engine {
		case "postgres":
			m.dropStaleSlots(ctx, *r)
		case "valkey":
			m.refollow(ctx, *r)
		}
	}
	if r.Applied != nil || r.Layout != currentLayout {
		r.Applied, r.Layout = nil, currentLayout
		if err := writeJSON(m.recordPath(r.Name), *r); err != nil {
			return err
		}
	}
	if r.PublicAccess {
		if err := m.applyGateway(ctx, *r, *r, true); err != nil {
			return fmt.Errorf("the database is running, but its public port did not open: %w", err)
		}
	}
	if r.Pooler.on() {
		if err := m.applyPooler(ctx, *r, *r, true); err != nil {
			return fmt.Errorf("the database is running, but its connection pooler did not start: %w", err)
		}
	}
	return nil
}

// Stop the database, and its gateway and pooler so nothing listens for it.
func (m *manager) stop(ctx context.Context, r record) error {
	services := []string{"db"}
	if r.PublicAccess {
		services = append(services, "gateway")
	}
	if r.Pooler.on() {
		services = append(services, "pooler")
	}
	return m.compose(ctx, r, nil, nil, append([]string{"stop", "--timeout", stopWait}, services...)...)
}

func live(state string) bool {
	return state == "running" || state == "starting" || state == "unhealthy" || state == "restarting"
}

func (m *manager) change(ctx context.Context, name, action string) (err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	var label string
	switch action {
	case "start":
		label = "Start database"
	case "stop":
		label = "Stop database"
	case "reboot":
		label = "Reboot database"
		states, err := m.states(ctx)
		if err != nil {
			return fail(503, "Cannot read database status: "+err.Error())
		}
		if !live(states[r.Project]) {
			return fail(409, "The database is not running. Start it instead.")
		}
	default:
		return fail(404, "Unknown database action.")
	}
	finish := m.track(ctx, name, label)
	defer func() { finish(err) }()
	switch action {
	case "start":
		err = m.start(ctx, &r)
	case "stop":
		err = m.stop(ctx, r)
	case "reboot":
		if err = m.stop(ctx, r); err == nil {
			err = m.start(ctx, &r)
		}
	}
	if err != nil {
		return fail(502, label+" failed: "+err.Error())
	}
	return nil
}

// Delete an instance and its data volume, optionally after a final snapshot.
func (m *manager) remove(ctx context.Context, name string, finalBackup bool) (err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	if r.DeletionProtection {
		return fail(409, "Deletion protection is on for this database. Modify the database to turn it off, then delete it.")
	}
	if reps, err := m.replicasOf(name); err != nil {
		return err
	} else if len(reps) > 0 {
		return fail(409, fmt.Sprintf("%s has read replicas: %s. Promote or delete them first.", name, replicaNames(reps)))
	}
	if finalBackup {
		if r.PendingRestore {
			return fail(409, "This instance has an incomplete restore, so it has no data to back up. Delete it without a final snapshot.")
		}
		finish := m.track(ctx, name, "Final snapshot")
		_, err := m.snapshot(ctx, r, "manual", true, "", false)
		finish(err)
		if err != nil {
			return fail(502, "The final snapshot failed, so the database was not deleted. Start the database to take one, or delete it without a final snapshot. "+describe(err))
		}
	}
	finish := m.track(ctx, name, "Delete database")
	defer func() { finish(err) }()
	if r.DiskGiB > 0 {
		err = m.checkExistingVolume(ctx, r)
	}
	if err == nil {
		err = m.compose(ctx, r, nil, nil, "down", "--volumes", "--remove-orphans", "--timeout", "45")
	}
	if err != nil {
		return fail(502, "Delete database failed: "+err.Error())
	}
	if r.DiskGiB > 0 {
		if err := m.detachDisk(ctx, r); err != nil {
			return fail(502, "The database was removed, but its disk image is still attached. Retry deletion. "+err.Error())
		}
	}
	if r.ReplicaOf != "" {
		m.forgetReplication(r)
		if src, err := m.record(r.ReplicaOf); err == nil && r.Engine == "postgres" {
			if err := m.dropSlot(ctx, src, slotName(r)); err != nil {
				// Dropped when the source next starts, or limited by max_slot_wal_keep_size meanwhile.
				fmt.Fprintf(os.Stderr, "removing the replication slot of %s from %s: %v\n", r.Name, src.Name, err)
			}
		}
	}
	m.history.forget(r.Project)
	m.forgetTLS(r)
	m.docker.run(ctx, nil, nil, nil, "volume", "rm", "--force", r.Project+"_archive") // WAL not yet collected
	if err := os.Remove(m.overridePath(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(m.recordPath(name))
}

// A partial update. Omitted fields keep their value; tags and parameters are
// replaced as a whole when present.
type modification struct {
	CPUs                *float64          `json:"cpus"`
	MemoryMiB           *int              `json:"memoryMiB"`
	Port                *int              `json:"port"`
	BackupRetentionDays *int              `json:"backupRetentionDays"`
	BackupWindow        *string           `json:"backupWindow"`
	BackupIntervalHours *int              `json:"backupIntervalHours"`
	DeletionProtection  *bool             `json:"deletionProtection"`
	Tags                map[string]string `json:"tags"`
	Parameters          map[string]string `json:"parameters"`
	RequireTLS          *bool             `json:"requireTLS"`
	PublicAccess        *bool             `json:"publicAccess"`
	PublicPort          *int              `json:"publicPort"`
	PublicHost          *string           `json:"publicHost"`
	Firewall            *[]firewallRule   `json:"firewall"`
	Pooler              *poolerSpec       `json:"pooler"` // replaced as a whole; {"enabled": false} turns it off
	PITR                *bool             `json:"pitr"`
	ApplyImmediately    bool              `json:"applyImmediately"`
}

type modifyResult struct {
	Restarted      bool `json:"restarted"`
	PendingRestart bool `json:"pendingRestart"`
}

// Settings that only need a record update apply at once. CPU, memory,
// parameters, TLS and the public address (named in the certificate) need a
// new container: a running database restarts now when asked, otherwise at its
// next restart. A port change always applies immediately, as in RDS. Public
// access and firewall rules apply at once through the gateway.
func (m *manager) modify(ctx context.Context, name string, mod modification) (result modifyResult, err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return result, err
	}
	defer release()
	if r.PendingRestore {
		return result, fail(409, "This instance has an incomplete restore.")
	}
	next := r
	next.Tags, next.Parameters = maps.Clone(r.Tags), maps.Clone(r.Parameters)
	if mod.CPUs != nil {
		next.CPUs = *mod.CPUs
	}
	if mod.MemoryMiB != nil {
		next.MemoryMiB = *mod.MemoryMiB
	}
	if mod.Port != nil {
		next.Port = *mod.Port
	}
	if mod.BackupRetentionDays != nil {
		next.BackupRetentionDays = *mod.BackupRetentionDays
	}
	if mod.BackupWindow != nil {
		next.BackupWindow = *mod.BackupWindow
	}
	if mod.BackupIntervalHours != nil {
		next.BackupIntervalHours = *mod.BackupIntervalHours
	}
	if mod.DeletionProtection != nil {
		next.DeletionProtection = *mod.DeletionProtection
	}
	if mod.Tags != nil {
		next.Tags = mod.Tags
	}
	if mod.Parameters != nil {
		next.Parameters = mod.Parameters
	}
	if mod.RequireTLS != nil {
		next.RequireTLS = *mod.RequireTLS
	}
	if mod.PublicAccess != nil {
		next.PublicAccess = *mod.PublicAccess
	}
	if mod.PublicPort != nil {
		next.PublicPort = *mod.PublicPort
	}
	if mod.PublicHost != nil {
		next.PublicHost = strings.TrimSpace(*mod.PublicHost)
	}
	if mod.Firewall != nil {
		rules, err := normalizeFirewall(*mod.Firewall)
		if err != nil {
			return result, err
		}
		next.Firewall = rules
	}
	if mod.PITR != nil {
		next.PITR = *mod.PITR
	}
	if mod.Pooler != nil {
		p := *mod.Pooler
		if p.Enabled && p.Port == 0 && r.Pooler.on() {
			p.Port = r.Pooler.Port // a change of settings keeps the port
		}
		next.Pooler = &p
	}
	if len(next.Tags) == 0 {
		next.Tags = nil
	}
	if len(next.Parameters) == 0 {
		next.Parameters = nil
	}
	if next.BackupWindow == "" {
		next.BackupWindow = defaultBackupWindow
	}
	if next.BackupIntervalHours == 24 {
		next.BackupIntervalHours = 0
	}
	all, err := m.records()
	if err != nil {
		return result, err
	}
	if next.PublicAccess && next.PublicPort == 0 {
		next.PublicPort = suggestPublicPort(all, next.instanceSpec)
	}
	normalizePooler(&next.instanceSpec, all)
	if err := checkSpec(next.instanceSpec); err != nil {
		return result, err
	}
	if err := checkReplication(all, next); err != nil {
		return result, err
	}
	if err := checkPorts(all, next.instanceSpec); err != nil {
		return result, err
	}
	previous := r.instanceSpec
	if previous.BackupWindow == "" {
		previous.BackupWindow = defaultBackupWindow // records from before backup windows existed
	}
	before, _ := json.Marshal(previous)
	after, _ := json.Marshal(next.instanceSpec)
	if bytes.Equal(before, after) {
		return result, nil
	}
	recreate := !next.runtime().equal(r.runtime()) || next.Port != r.Port
	gatewayChanged := next.PublicAccess != r.PublicAccess ||
		next.PublicAccess && (next.PublicPort != r.PublicPort || allowList(next.Firewall) != allowList(r.Firewall))
	poolerChanged := !samePooler(next.Pooler, r.Pooler)
	running := false
	if recreate || gatewayChanged || poolerChanged {
		states, err := m.states(ctx)
		if err != nil {
			return result, fail(503, "Cannot read database status: "+err.Error())
		}
		running = live(states[r.Project])
	}
	action := "Modify database"
	switch {
	case !recreate && gatewayChanged:
		action = "Change network access"
	case !recreate && poolerChanged:
		action = "Change connection pooling"
	}
	finish := m.track(ctx, name, action)
	defer func() { finish(err) }()
	switch {
	case running && (mod.ApplyImmediately || next.Port != r.Port):
		next.Applied = nil
		if err = m.save(next); err != nil {
			return result, err
		}
		if err = m.start(ctx, &next); err != nil {
			return result, m.rollback(ctx, r, err)
		}
		result.Restarted = true
		if next.PublicAccess {
			gatewayChanged = false // start brought the gateway up with the new settings
		}
	case running:
		if next.Applied == nil {
			current := r.runtime()
			next.Applied = &current
		}
		if next.Applied.equal(next.runtime()) {
			next.Applied = nil // the change was reverted before it was applied
		}
		if err = m.save(next); err != nil {
			return result, err
		}
		result.PendingRestart = next.Applied != nil
	default:
		if err = m.save(next); err != nil {
			return result, err
		}
	}
	if gatewayChanged {
		if err = m.applyGateway(ctx, r, next, running); err != nil {
			// Put the previous network settings back, so the record matches what runs.
			back := next
			back.PublicAccess, back.PublicPort, back.Firewall = r.PublicAccess, r.PublicPort, r.Firewall
			if saveErr := m.save(back); saveErr == nil {
				m.applyGateway(ctx, next, back, running)
			}
			return result, fail(502, "Network access did not change: the public port could not be opened. "+err.Error())
		}
	}
	if poolerChanged {
		if err = m.applyPooler(ctx, r, next, running); err != nil {
			// Put the previous pooler back, so the record matches what runs.
			back := next
			back.Pooler = r.Pooler
			if saveErr := m.save(back); saveErr == nil {
				m.applyPooler(ctx, next, back, running)
			}
			return result, fail(502, "Connection pooling did not change: "+err.Error())
		}
	}
	if next.BackupRetentionDays != r.BackupRetentionDays {
		if err = m.retain(next); err != nil {
			return result, err
		}
	}
	return result, nil
}

// The engine's usual port when it is free, otherwise the next free one.
func suggestPublicPort(all []record, s instanceSpec) int {
	for port := engines[s.Engine].port; port < 65535; port++ {
		probe := s
		probe.PublicAccess, probe.PublicPort = true, port
		if port != s.Port && checkPorts(all, probe) == nil {
			return port
		}
	}
	return 0
}

// Restore the previous configuration after a change that did not start, and
// bring the database back with it.
func (m *manager) rollback(ctx context.Context, previous record, cause error) error {
	previous.Applied = nil
	if err := writeJSON(m.recordPath(previous.Name), previous); err != nil {
		return fail(502, "The change did not apply and the previous settings could not be saved. "+cause.Error())
	}
	if err := m.start(ctx, &previous); err != nil {
		return fail(502, "The change did not apply, and the database did not restart with its previous settings. Check its logs. "+cause.Error())
	}
	return fail(502, "The change did not apply, so the database is running with its previous settings again. "+cause.Error())
}

type usage struct {
	CPU     string `json:"CPUPerc"`
	Memory  string `json:"MemUsage"`
	Percent string `json:"MemPerc"`
	Net     string `json:"NetIO"`
	Disk    string `json:"BlockIO"`
}

type hostInfo struct {
	Name        string       `json:"name"`
	CPUs        int          `json:"cpus"`
	MemoryBytes int64        `json:"memoryBytes"`
	Version     string       `json:"dockerVersion"`
	VolumeQuota quotaSupport `json:"volumeQuota"`
	BackupDisk  *diskInfo    `json:"backupDisk,omitempty"`
	// Notification channels set up, so the dashboard can suggest one.
	NotificationChannels int `json:"notificationChannels"`
}

// Space on the disk that holds backups. Backups stop below the reserve.
type diskInfo struct {
	FreeBytes    uint64 `json:"freeBytes"`
	TotalBytes   uint64 `json:"totalBytes"`
	ReserveBytes uint64 `json:"reserveBytes"`
}

func (m *manager) host(ctx context.Context) (hostInfo, error) {
	var out bytes.Buffer
	var host hostInfo
	format := `{"name":{{json .Name}},"cpus":{{.NCPU}},"memoryBytes":{{.MemTotal}},"dockerVersion":{{json .ServerVersion}}}`
	if err := m.docker.run(ctx, nil, nil, &out, "info", "--format", format); err != nil {
		return host, fail(503, "Cannot read host capacity: "+err.Error())
	}
	err := json.Unmarshal(out.Bytes(), &host)
	if err == nil {
		host.VolumeQuota = m.volumeQuota(ctx)
	}
	if free, total, ok := m.space(m.backupDir()); ok {
		host.BackupDisk = &diskInfo{free, total, backupReserve(total)}
	}
	if cfg, err := m.loadNotify(); err == nil {
		host.NotificationChannels = len(cfg.Channels)
	}
	return host, err
}

func (m *manager) metrics(ctx context.Context, name string) (*usage, error) {
	r, err := m.record(name)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := m.compose(ctx, r, nil, &out, "ps", "--quiet", "--status", "running", "db"); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(out.String())
	if id == "" {
		return nil, nil // stopped or not yet created; no live sample
	}
	out.Reset()
	if err := m.docker.run(ctx, nil, nil, &out, "stats", "--no-stream", "--format", "{{json .}}", id); err != nil {
		return nil, err
	}
	var u usage
	if err := json.Unmarshal(out.Bytes(), &u); err != nil {
		return nil, fmt.Errorf("reading Docker stats: %w", err)
	}
	return &u, nil
}

func (m *manager) logs(ctx context.Context, name string) (string, error) {
	r, err := m.record(name)
	if err != nil {
		return "", err
	}
	out := tailBuffer{limit: 64 << 10}
	if err := m.compose(ctx, r, nil, &out, "logs", "--no-color", "--no-log-prefix", "--tail", "120", "--timestamps", "db"); err != nil {
		return "", err
	}
	text := string(out.data)
	secrets := []string{r.Password, r.RootPassword, r.ReplicationPassword}
	for _, u := range r.Users {
		secrets = append(secrets, u.Password)
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text, nil
}
