package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxDiskGiB = 65536

func quotaBytes(gib int) string { return strconv.FormatInt(int64(gib)<<30, 10) }

type quotaSupport struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

type quotaCache struct {
	mu      sync.Mutex
	support quotaSupport
	expires time.Time
}

const diskImageVolume = "openrds_disk_images"

func diskHelperImage() string {
	return env("OPENRDS_DISK_HELPER_IMAGE", "openrds-disk-helper:local")
}

// A Docker volume has no portable size field. The workable approach is a
// fixed-size ext4 image, attached by a privileged helper and mounted by the
// ordinary local volume driver.
func (m *manager) volumeQuota(ctx context.Context) quotaSupport {
	c := &m.quotaCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expires) {
		return c.support
	}
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	image := diskHelperImage()
	err := m.docker.run(probe, nil, nil, nil, "image", "inspect", "--format", "{{.Id}}", image)
	switch {
	case err != nil && strings.Contains(err.Error(), "No such image"):
		c.support = quotaSupport{"unsupported", "The disk helper image is not installed. Run sh install.sh on the VPS."}
	case err != nil:
		c.support = quotaSupport{"unknown", "Could not check disk limits: " + err.Error()}
	default:
		err = m.docker.run(probe, nil, nil, nil, "run", "--rm", "--privileged", "--pid=host", image, "check")
		if err != nil {
			c.support = quotaSupport{"unsupported", "This Docker daemon cannot attach loop devices, so a hard disk limit is unavailable. Use rootful Docker on Linux. Helper said: " + err.Error()}
		} else {
			c.support = quotaSupport{"supported", "A disk limit is a fixed-size ext4 Docker volume. Usable space is slightly less than the size you choose."}
		}
	}
	ttl := 10 * time.Minute
	if c.support.State == "unknown" {
		ttl = 30 * time.Second
	}
	c.expires = time.Now().Add(ttl)
	return c.support
}

func (m *manager) diskImagePath(ctx context.Context, project string) (string, error) {
	if err := m.docker.run(ctx, nil, nil, nil, "volume", "create", "--label", "io.openrds.disk-images=true", diskImageVolume); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := m.docker.run(ctx, nil, nil, &out, "volume", "inspect", "--format", "{{.Mountpoint}}", diskImageVolume); err != nil {
		return "", err
	}
	mountpoint := strings.TrimSpace(out.String())
	if mountpoint == "" || strings.Contains(mountpoint, "..") || !strings.HasSuffix(mountpoint, "/"+diskImageVolume+"/_data") {
		return "", fmt.Errorf("unexpected disk-image volume path %q", mountpoint)
	}
	return mountpoint + "/" + project + ".img", nil
}

func (m *manager) diskHelper(ctx context.Context, command string, r record, gib int) error {
	image, err := m.diskImagePath(ctx, r.Project)
	if err != nil {
		return err
	}
	return m.docker.run(ctx, nil, nil, nil, "run", "--rm", "--privileged", "--pid=host",
		"--mount", "type=volume,src="+diskImageVolume+",dst=/images",
		"--env", "OPENRDS_LINK="+r.Project,
		"--env", "OPENRDS_BYTES="+quotaBytes(gib),
		"--env", "OPENRDS_IMAGE="+image,
		diskHelperImage(), command)
}

func (m *manager) attachDisk(ctx context.Context, r record) error {
	return m.diskHelper(ctx, "attach", r, r.DiskGiB)
}

func (m *manager) detachDisk(ctx context.Context, r record) error {
	return m.diskHelper(ctx, "detach", r, r.DiskGiB)
}

// Reattach capped volumes after a daemon restart. Loop devices do not survive
// a reboot, while Docker may retry the database container before this process starts.
func (m *manager) prepareDisks(ctx context.Context) {
	records, err := m.records()
	if err != nil {
		log.Printf("preparing disk limits: %v", err)
		return
	}
	for _, r := range records {
		if r.DiskGiB == 0 {
			continue
		}
		if err := m.attachDisk(ctx, r); err != nil {
			log.Printf("disk limit for %s is not attached: %v", r.Name, err)
		}
	}
}

// Capped databases that Docker tried to start while their disk image was not
// attached yet, for example right after Docker Desktop or the server
// restarted. Docker records the failed mount and does not try again. A
// database stopped on purpose has no such error, so it stays stopped.
func (m *manager) failedMounts(ctx context.Context) []record {
	records, err := m.records()
	if err != nil {
		log.Printf("checking disk limits after a restart: %v", err)
		return nil
	}
	var failed []record
	for _, r := range records {
		if r.DiskGiB == 0 || r.PendingRestore {
			continue
		}
		var out bytes.Buffer
		err := m.docker.run(ctx, nil, nil, &out, "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+r.Project,
			"--filter", "label=io.openrds.database=true")
		ids := strings.Fields(out.String())
		if err != nil || len(ids) != 1 { // exactly one database container
			continue
		}
		id := ids[0]
		out.Reset()
		if err := m.docker.run(ctx, nil, nil, &out, "inspect", "--format", "{{.State.Status}}|{{.State.Error}}", id); err != nil {
			continue
		}
		state, problem, _ := strings.Cut(strings.TrimSpace(out.String()), "|")
		if state == "exited" && strings.Contains(problem, "mounting volume") {
			failed = append(failed, r)
		}
	}
	return failed
}

// Start the databases Docker could not, once their disk images are attached again.
func (m *manager) recoverDisks(ctx context.Context) {
	for _, r := range m.failedMounts(ctx) {
		current, release, err := m.begin(r.Name, nil, false)
		if err != nil {
			continue
		}
		finish := m.track(ctx, r.Name, "Start database after Docker restart")
		err = m.start(ctx, &current)
		finish(err)
		release()
		if err != nil {
			log.Printf("starting %s after a Docker restart: %v", r.Name, err)
		}
	}
}

func (m *manager) checkDiskRequest(ctx context.Context, gib int) error {
	if gib == 0 {
		return nil
	}
	if gib < 0 || gib > maxDiskGiB {
		return fail(400, fmt.Sprintf("Choose a data-volume limit of 1–%d GiB, or zero for uncapped.", maxDiskGiB))
	}
	support := m.volumeQuota(ctx)
	switch support.State {
	case "supported":
		return nil
	case "unsupported":
		return fail(422, support.Message)
	default:
		return fail(503, support.Message)
	}
}

// Compose can reuse a pre-existing volume by name. Never adopt an uncapped,
// differently sized, or unrelated volume for a capped instance.
func (m *manager) checkExistingVolume(ctx context.Context, r record) error {
	name := r.Project + "_data"
	var out bytes.Buffer
	if err := m.docker.run(ctx, nil, nil, &out, "volume", "ls", "--quiet", "--filter", "name=^"+name+"$"); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) == "" {
		return nil // the quota overlay will create it
	}
	out.Reset()
	format := `{{.Driver}}|{{index .Options "type"}}|{{index .Options "device"}}|{{index .Labels "com.docker.compose.project"}}|{{index .Labels "com.docker.compose.volume"}}`
	if err := m.docker.run(ctx, nil, nil, &out, "volume", "inspect", "--format", format, name); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) != "local|ext4|/dev/openrds/"+r.Project+"|"+r.Project+"|data" {
		return fmt.Errorf("existing volume %s does not match this instance’s capped ext4 volume; it was left intact", name)
	}
	return nil
}

// Measure only on request. A recursive scan on every metrics poll would be
// expensive for an instance with many data files.
func (m *manager) volumeUsage(ctx context.Context, name string) (int64, error) {
	r, err := m.record(name)
	if err != nil {
		return 0, err
	}
	scan, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var out bytes.Buffer
	if err := m.compose(scan, r, nil, &out, "exec", "-T", "db", "du", "-skx", r.dataMount()); err != nil {
		return 0, fail(502, "Could not measure the running database’s data volume: "+err.Error())
	}
	fields := strings.Fields(out.String())
	if len(fields) < 2 {
		return 0, fmt.Errorf("missing data volume usage")
	}
	kib, err := strconv.ParseInt(fields[0], 10, 54)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("invalid data volume usage")
	}
	return kib * 1024, nil
}
