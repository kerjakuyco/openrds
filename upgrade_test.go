package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestDataGenerationsNameTheirVolumeAndDisk(t *testing.T) {
	r := record{Project: "openrds-db-abc"}
	if r.dataVolume() != "openrds-db-abc_data" || r.diskKey() != "openrds-db-abc" {
		t.Fatal("the first generation keeps the names from before upgrades")
	}
	r.DataGeneration = 2
	if r.dataVolume() != "openrds-db-abc_data2" || r.diskKey() != "openrds-db-abc-2" {
		t.Fatalf("generation 2: %s %s", r.dataVolume(), r.diskKey())
	}
}

func TestUpgradeChecksTheVersion(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Version: "17", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"16", "17", "14", "99"} {
		if err := m.upgrade(ctx, "orders", v); status(err) != 400 {
			t.Errorf("version %s: %v", v, err)
		}
	}
	if err := m.change(ctx, "orders", "stop"); err != nil {
		t.Fatal(err)
	}
	if err := m.upgrade(ctx, "orders", "18"); status(err) != 409 || !strings.Contains(err.Error(), "Start orders") {
		t.Fatalf("a stopped database cannot be copied: %v", err)
	}
}

// Which Compose files and verb a call used.
func callShape(args []string) (verb string, staging bool) {
	return composeVerb(args), strings.Contains(strings.Join(args, " "), "upgrade.yaml")
}

func TestUpgradeKeepsTheEndpointAndData(t *testing.T) {
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Version: "17", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.createUser(ctx, "orders", "reporting", "read-only", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.modify(ctx, "orders", modification{Pooler: &poolerSpec{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	before, _ := m.record("orders")
	f.mu.Lock()
	from := len(f.args)
	f.mu.Unlock()

	if err := m.upgrade(ctx, "orders", "19"); err != nil {
		t.Fatal(err)
	}
	after, _ := m.record("orders")
	if after.Version != "19" || after.DataGeneration != 1 || after.Project != before.Project || after.Port != before.Port ||
		after.Password != before.Password || len(after.Users) != 1 || !after.Pooler.on() {
		t.Fatalf("the endpoint, credentials, users and pooler stay; the version and volume change: %+v", after)
	}

	f.mu.Lock()
	calls := f.args[from:]
	inputs := strings.Join(f.inputs, "\n")
	f.mu.Unlock()
	var steps []string
	for _, c := range calls {
		verb, staging := callShape(c)
		step := verb
		if verb == "up" || verb == "stop" {
			step += " " + c[len(c)-1] // the service
		}
		if staging {
			step = strings.Replace(step, verb, verb+" (private)", 1)
		}
		if len(steps) == 0 || steps[len(steps)-1] != step {
			steps = append(steps, step)
		}
	}
	want := []string{"exec", "stop pooler", "exec", "stop db", "up (private) db", "exec (private)", "up db", "up pooler"}
	if strings.Join(steps, ", ") != strings.Join(want, ", ") {
		t.Fatalf("steps:\n got %v\nwant %v", steps, want)
	}
	if !strings.Contains(inputs, "local all all trust") || !strings.Contains(inputs, `CREATE ROLE "reporting"`) {
		t.Fatal("clients are held off by local-only access, and users are recreated in the new version")
	}
	data, _ := os.ReadFile(m.overridePath(after))
	if !strings.Contains(string(data), `"name":"`+after.Project+`_data1"`) {
		t.Fatalf("the new version uses its own volume: %s", data)
	}
	list, _ := m.backups()
	if len(list) != 1 || !strings.Contains(list[0].Note, "Before the upgrade to PostgreSQL 19") || list[0].Version != "17" {
		t.Fatalf("a snapshot of the old version is kept: %+v", list)
	}
	items, _ := m.readActivity()
	found := false
	for _, e := range items {
		found = found || e.Action == "Upgrade engine version" && e.Result == "completed" && e.Detail == "PostgreSQL 17 → 19"
	}
	if !found {
		t.Fatal("the upgrade is in Events")
	}
}

func TestAFailedUpgradeGoesBackToTheOldVersion(t *testing.T) {
	f := newFakeDocker()
	var removed []string
	m := testManager(t, func(c context.Context, env []string, in io.Reader, out io.Writer, args ...string) error {
		if args[0] == "volume" && args[1] == "rm" {
			removed = append(removed, args[len(args)-1])
		}
		if verb, staging := callShape(args); args[0] == "compose" && verb == "exec" && staging && in != nil {
			io.Copy(io.Discard, in)
			return errors.New("pg_restore: error: could not execute query")
		}
		return f.run(c, env, in, out, args...)
	})
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Version: "17", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := m.record("orders")
	err := m.upgrade(ctx, "orders", "18")
	if status(err) != 502 || !strings.Contains(err.Error(), "runs PostgreSQL 17 again") {
		t.Fatalf("a failed restore reports the way back: %v", err)
	}
	after, _ := m.record("orders")
	if after.Version != "17" || after.DataGeneration != 0 {
		t.Fatalf("the record keeps the old version and volume: %+v", after)
	}
	if len(removed) != 1 || removed[0] != before.Project+"_data1" {
		t.Fatalf("only the new version's volume is removed: %v", removed)
	}
	last := f.args[len(f.args)-1]
	if verb, staging := callShape(last); verb != "up" || staging || f.lastUp("OPENRDS_DB_IMAGE") != "postgres:17-alpine" {
		t.Fatalf("the old version starts again on its own port: %v %s", last, f.lastUp("OPENRDS_DB_IMAGE"))
	}
	if list, _ := m.backups(); len(list) != 1 {
		t.Fatal("the snapshot from before the upgrade is kept")
	}
}
