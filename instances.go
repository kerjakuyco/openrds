package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type instanceView struct {
	instanceSpec
	EngineLabel    string    `json:"engineLabel"`
	Status         string    `json:"status"`
	Busy           bool      `json:"busy"`
	Created        time.Time `json:"createdAt"`
	PendingRestore bool      `json:"pendingRestore"`
	User           string    `json:"user"`
	Database       string    `json:"database"`
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
	result := make([]instanceView, 0, len(all))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range all {
		state := states[r.Project]
		if state == "" {
			state = "not provisioned"
		}
		eng := engines[r.Engine]
		result = append(result, instanceView{r.instanceSpec, eng.title, state,
			m.busy[r.Name], r.Created, r.PendingRestore, eng.user, eng.database})
	}
	return result, nil
}

// Retain metadata (and any provisioned volume) on Docker failure so the
// operator can retry Start or explicitly delete the instance.
func (m *manager) provision(ctx context.Context, spec instanceSpec, from *backup) (err error) {
	if err := m.checkDiskRequest(ctx, spec.DiskGiB); err != nil {
		return err // reject before reserving a name/port or touching an instance
	}
	var input *os.File
	if from != nil {
		spec.Engine = from.Engine
		input, err = m.openDump(from.ID)
		if err != nil {
			return err
		}
		defer input.Close()
	}
	r, release, err := m.begin(spec.Name, &spec, from != nil)
	if err != nil {
		return err
	}
	defer release()
	action := "Provision database"
	if from != nil {
		action = "Restore backup"
	}
	finish := m.track(r.Name, action)
	defer func() { finish(err) }()
	if err := m.start(ctx, r); err != nil {
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

func (m *manager) restoreInto(ctx context.Context, r record, input *os.File) error {
	eng := engines[r.Engine]
	if !eng.restoreStopped {
		args := append([]string{"exec", "-T", "db"}, eng.restore...)
		return m.compose(ctx, r, input, nil, args...)
	}
	if err := m.compose(ctx, r, nil, nil, "stop", "--timeout", "30", "db"); err != nil {
		return err
	}
	script := eng.restore[len(eng.restore)-1]
	if err := m.compose(ctx, r, input, nil, "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "db", "-ec", script); err != nil {
		return err
	}
	return m.compose(ctx, r, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "180", "db")
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
	finish := m.track(name, "Resize disk")
	defer func() { finish(err) }()
	if err = m.diskHelper(ctx, "resize", r, gib); err != nil {
		return fail(502, "Disk resize did not finish. "+err.Error())
	}
	r.DiskGiB = gib
	return writeJSON(m.recordPath(name), r)
}

func (m *manager) start(ctx context.Context, r record) error {
	if r.DiskGiB > 0 {
		if err := m.checkDiskRequest(ctx, r.DiskGiB); err != nil {
			return err
		}
		if err := m.checkExistingVolume(ctx, r); err != nil {
			return err
		}
		if err := m.attachDisk(ctx, r); err != nil {
			return err
		}
	}
	return m.compose(ctx, r, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "180", "db")
}

func (m *manager) change(ctx context.Context, name, action string) (err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	var args []string
	var label string
	switch action {
	case "start":
		label = "Start database"
	case "stop":
		label, args = "Stop database", []string{"stop", "--timeout", "45", "db"}
	case "delete":
		label, args = "Delete database", []string{"down", "--volumes", "--timeout", "45"}
	default:
		return fail(404, "Unknown database action.")
	}
	finish := m.track(name, label)
	defer func() { finish(err) }()
	if action == "start" {
		err = m.start(ctx, r)
	} else {
		if action == "delete" && r.DiskGiB > 0 {
			err = m.checkExistingVolume(ctx, r)
		}
		if err == nil {
			err = m.compose(ctx, r, nil, nil, args...)
		}
	}
	if err != nil {
		return fail(502, label+" failed: "+err.Error())
	}
	if action == "delete" {
		if r.DiskGiB > 0 {
			if err := m.detachDisk(ctx, r); err != nil {
				return fail(502, "The database was removed, but its disk image is still attached. Retry deletion. "+err.Error())
			}
		}
		return os.Remove(m.recordPath(name))
	}
	return nil
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
	for _, secret := range []string{r.Password, r.RootPassword} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text, nil
}
