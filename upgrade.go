package main

// Engine version upgrades that keep the endpoint, like RDS's "Modify engine
// version": the database keeps its name, port, Docker network, credentials,
// users and settings. Backups are logical, so a new version gets a fresh data
// volume and the data moves by dump and restore. The old volume stays
// untouched until the new version runs, so a failure puts the old version
// back at once, and a snapshot from just before is kept either way.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// The data volume and disk-image key of the record's data generation.
// Generation 0 keeps the names from before upgrades existed.
func (r record) dataVolume() string {
	if r.DataGeneration == 0 {
		return r.Project + "_data"
	}
	return fmt.Sprintf("%s_data%d", r.Project, r.DataGeneration)
}

func (r record) diskKey() string {
	if r.DataGeneration == 0 {
		return r.Project
	}
	return fmt.Sprintf("%s-%d", r.Project, r.DataGeneration)
}

// Keep clients out while the data is copied, so nothing written after the
// dump is lost: PostgreSQL accepts only local connections, MySQL goes into
// offline mode and Valkey pauses writes. OpenRDS's own connections are local
// or administrative, so the dump still works.
func (m *manager) blockClients(ctx context.Context, r record) error {
	switch r.Engine {
	case "postgres":
		script := `cat > /run/openrds/pg_hba.conf
psql -X -q -v ON_ERROR_STOP=1 -U openrds -d app -c "SELECT pg_reload_conf()" -c "SELECT pg_sleep(1)" \
  -c "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()"`
		return m.compose(ctx, r, strings.NewReader("local all all trust\n"), io.Discard, "exec", "-T", "db", "sh", "-ec", script)
	case "mysql":
		return m.admin(ctx, r, "SET GLOBAL offline_mode = ON;\n")
	default:
		return m.admin(ctx, r, "CLIENT PAUSE 3600000 WRITE\n")
	}
}

// Let clients back in when the upgrade stops before the database was stopped.
func (m *manager) unblockClients(ctx context.Context, r record) error {
	switch r.Engine {
	case "postgres":
		script := `cat > /run/openrds/pg_hba.conf
psql -X -q -v ON_ERROR_STOP=1 -U openrds -d app -c "SELECT pg_reload_conf()"`
		return m.compose(ctx, r, strings.NewReader(pgHBA(r)), io.Discard, "exec", "-T", "db", "sh", "-ec", script)
	case "mysql":
		return m.admin(ctx, r, "SET GLOBAL offline_mode = OFF;\n")
	default:
		return m.admin(ctx, r, "CLIENT UNPAUSE\n")
	}
}

// Remove a data generation's volume, and its disk image when capped.
func (m *manager) dropData(ctx context.Context, r record) error {
	if err := m.docker.run(ctx, nil, nil, nil, "volume", "rm", "--force", r.dataVolume()); err != nil {
		return err
	}
	if r.DiskGiB > 0 {
		return m.detachDisk(ctx, r)
	}
	return nil
}

func (m *manager) upgrade(ctx context.Context, name, version string) (err error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return err
	}
	defer release()
	title := engines[r.Engine].title
	switch {
	case !offeredVersion(r.Engine, version):
		return fail(400, fmt.Sprintf("Choose a listed %s version.", title))
	case compareVersions(version, r.version()) <= 0:
		return fail(400, fmt.Sprintf("Choose a version newer than %s %s. Versions cannot go back: restore a snapshot into a new database instead.", title, r.version()))
	case r.PendingRestore:
		return fail(409, "This instance has an incomplete restore.")
	case r.Layout < currentLayout:
		return fail(409, fmt.Sprintf("Reboot %s once first. It was started by an earlier version of OpenRDS.", r.Name))
	}
	states, err := m.states(ctx)
	if err != nil {
		return fail(503, "Cannot read database status: "+err.Error())
	}
	if states[r.Project] != "running" {
		return fail(409, fmt.Sprintf("Start %s and wait until it is healthy: the upgrade copies its data while it runs.", r.Name))
	}
	if r.DiskGiB > 0 {
		if err := m.checkDiskRequest(ctx, r.DiskGiB); err != nil {
			return err // the new version gets its own disk while the old one is kept
		}
	}
	finish := m.trackDetail(ctx, name, "Upgrade engine version", fmt.Sprintf("%s %s → %s", title, r.version(), version))
	defer func() { finish(err) }()
	// Download the new version first, so it does not add to the downtime.
	image := fmt.Sprintf(engineVersions[r.Engine].image, version)
	if m.docker.run(ctx, nil, nil, io.Discard, "image", "inspect", "--format", "{{.Id}}", image) != nil {
		if err := m.docker.run(ctx, nil, nil, io.Discard, "pull", "--quiet", image); err != nil {
			return fail(502, "The upgrade did not start: "+image+" could not be downloaded. "+err.Error())
		}
	}

	// 1. Clients out, then a snapshot of the data as it stands.
	if err := m.blockClients(ctx, r); err != nil {
		m.unblockClients(ctx, r)
		return fail(502, "The upgrade did not start: clients could not be held off. "+err.Error())
	}
	if r.PublicAccess || r.Pooler.on() {
		services := []string{}
		if r.PublicAccess {
			services = append(services, "gateway")
		}
		if r.Pooler.on() {
			services = append(services, "pooler")
		}
		m.compose(ctx, r, nil, nil, append([]string{"stop", "--timeout", "30"}, services...)...)
	}
	snap, err := m.snapshot(ctx, r, "manual", false, fmt.Sprintf("Before the upgrade to %s %s", title, version))
	if err != nil {
		m.unblockClients(ctx, r)
		m.start(ctx, &r) // brings the gateway and pooler back
		return fail(502, "The upgrade did not start: the snapshot before it failed, and nothing changed. "+describe(err))
	}

	// 2. The new version on a fresh volume, private until its data is in.
	staging := r
	staging.Version, staging.DataGeneration, staging.Applied = version, r.DataGeneration+1, nil
	staging.staging = true
	rollback := func(cause error) error {
		detail := describe(cause)
		var p *problem
		if !errors.As(cause, &p) {
			detail = cause.Error() // Docker's errors, with passwords already redacted
		}
		m.compose(ctx, staging, nil, nil, "rm", "--stop", "--force", "db")
		if err := m.dropData(ctx, staging); err != nil {
			fmt.Fprintf(os.Stderr, "removing the data of a failed upgrade of %s: %v\n", r.Name, err)
		}
		if err := m.start(ctx, &r); err != nil {
			return fail(502, fmt.Sprintf("The upgrade failed, and %s %s did not start again. Its data volume is intact; start it from the console. %s", title, r.version(), detail))
		}
		return fail(502, fmt.Sprintf("The upgrade did not finish, so %s runs %s %s again with all its data. The snapshot from before the upgrade is kept. %s", r.Name, title, r.version(), detail))
	}
	if err := m.compose(ctx, r, nil, nil, "stop", "--timeout", stopWait, "db"); err != nil {
		m.unblockClients(ctx, r)
		m.start(ctx, &r)
		return fail(502, "The upgrade did not start: the database could not be stopped. "+err.Error())
	}
	if r.DiskGiB > 0 {
		if err := m.attachDisk(ctx, staging); err != nil {
			return rollback(err)
		}
	}
	if err := m.up(ctx, staging); err != nil {
		return rollback(fmt.Errorf("the new version did not start: %w", err))
	}
	data, closer, err := m.openBackupData(snap)
	if err != nil {
		return rollback(err)
	}
	err = m.restoreInto(ctx, staging, data)
	closer.Close()
	if err != nil {
		return rollback(fmt.Errorf("restoring the data failed: %w", err))
	}
	if r.Engine != "valkey" { // Valkey users come from its users file
		for _, u := range r.Users {
			if err := m.admin(ctx, staging, grantScript(staging, u)); err != nil {
				return rollback(fmt.Errorf("recreating database user %s failed: %w", u.Name, err))
			}
		}
	}

	// 3. Back on its own port and name, with its gateway and pooler.
	final := staging
	final.staging = false
	if err := m.save(final); err != nil {
		return rollback(err)
	}
	if err := m.start(ctx, &final); err != nil {
		if saveErr := m.save(r); saveErr != nil {
			return fail(502, "The upgrade failed and the previous settings could not be saved: "+saveErr.Error())
		}
		return rollback(err)
	}
	if err := m.dropData(ctx, r); err != nil {
		m.note(ctx, name, "Remove the data of the previous version", "failed", "Volume "+r.dataVolume()+": "+err.Error())
	}
	return nil
}

func upgradeRoutes(add func(string, int, endpoint), m *manager) {
	add("POST /api/instances/{name}/upgrade", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Version string `json:"version"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		if err := m.upgrade(r.Context(), r.PathValue("name"), body.Version); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
