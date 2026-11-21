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
		result = append(result, instanceView{r.instanceSpec, engines[r.Engine].title, state,
			m.busy[r.Name], r.Created, r.PendingRestore})
	}
	return result, nil
}

// Retain metadata (and any provisioned volume) on Docker failure so the
// operator can retry Start or explicitly delete the instance.
func (m *manager) provision(ctx context.Context, spec instanceSpec, from *backup) (err error) {
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
	if err := m.compose(ctx, r, nil, nil, "up", "--detach", "--wait", "--wait-timeout", "180", "db"); err != nil {
		return fail(502, "Provisioning did not finish. The instance was kept for inspection. "+err.Error())
	}
	if from != nil {
		args := append([]string{"exec", "-T", "db"}, engines[r.Engine].restore...)
		if err := m.compose(ctx, r, input, nil, args...); err != nil {
			return fail(502, "Restore did not finish. The new instance is marked incomplete; the backup is intact. "+err.Error())
		}
		r.PendingRestore = false
		return writeJSON(m.recordPath(r.Name), r)
	}
	return nil
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
		label, args = "Start database", []string{"up", "--detach", "--wait", "--wait-timeout", "180", "db"}
	case "stop":
		label, args = "Stop database", []string{"stop", "--timeout", "45", "db"}
	case "delete":
		label, args = "Delete database", []string{"down", "--volumes", "--timeout", "45"}
	default:
		return fail(404, "Unknown database action.")
	}
	finish := m.track(name, label)
	defer func() { finish(err) }()
	if err := m.compose(ctx, r, nil, nil, args...); err != nil {
		return fail(502, label+" failed: "+err.Error())
	}
	if action == "delete" {
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
	Name        string `json:"name"`
	CPUs        int    `json:"cpus"`
	MemoryBytes int64  `json:"memoryBytes"`
	Version     string `json:"dockerVersion"`
}

func (m *manager) host(ctx context.Context) (hostInfo, error) {
	var out bytes.Buffer
	var host hostInfo
	format := `{"name":{{json .Name}},"cpus":{{.NCPU}},"memoryBytes":{{.MemTotal}},"dockerVersion":{{json .ServerVersion}}}`
	if err := m.docker.run(ctx, nil, nil, &out, "info", "--format", format); err != nil {
		return host, fail(503, "Cannot read host capacity: "+err.Error())
	}
	err := json.Unmarshal(out.Bytes(), &host)
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
