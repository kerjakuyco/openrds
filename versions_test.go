package main

import "testing"

func TestVersionsAreChosenKeptAndOnlyUpgraded(t *testing.T) {
	if compareVersions("9.10", "9.7") != 1 || compareVersions("8.4", "9.7") != -1 || compareVersions("18", "18.0") != 0 {
		t.Fatal("versions must compare numerically")
	}
	legacy := record{instanceSpec: instanceSpec{Engine: "postgres"}}
	if legacy.version() != "17" || legacy.image() != "postgres:17-alpine" || legacy.dataMount() != "/var/lib/postgresql/data" {
		t.Fatalf("a database from before versions must keep its image and data: %s %s", legacy.image(), legacy.dataMount())
	}
	if r := (record{instanceSpec: instanceSpec{Engine: "postgres", Version: "19"}}); r.image() != "postgres:19-alpine" || r.dataMount() != "/var/lib/postgresql" {
		t.Fatalf("PostgreSQL 18 and newer mount the parent folder: %s %s", r.image(), r.dataMount())
	}
	if r := (record{instanceSpec: instanceSpec{Engine: "mysql", Version: "9.7"}}); r.image() != "mysql:9.7" || r.dataMount() != "/var/lib/mysql" {
		t.Fatal("mysql image")
	}
	for engine := range engines {
		if !offeredVersion(engine, defaultVersion(engine)) {
			t.Errorf("%s: the default version must be listed", engine)
		}
	}

	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := m.record("orders")
	if r.Version != "18" || f.lastUp("OPENRDS_DB_IMAGE") != "postgres:18-alpine" || f.lastUp("OPENRDS_DATA_MOUNT") != "/var/lib/postgresql" {
		t.Fatalf("a new database gets the stable version: %q %s", r.Version, f.lastUp("OPENRDS_DB_IMAGE"))
	}
	if err := m.provision(ctx, instanceSpec{Name: "old", Engine: "postgres", Version: "14", Port: 15433}, nil); status(err) != 400 {
		t.Fatalf("an unlisted version must be refused: %v", err)
	}
	if _, err := m.record("old"); status(err) != 404 {
		t.Fatal("a refused version must not leave a record behind")
	}
	b, err := m.makeBackup(ctx, "orders", "manual")
	if err != nil || b.Version != "18" {
		t.Fatalf("backups record their version: %+v %v", b, err)
	}
	if err := m.provision(ctx, instanceSpec{Name: "older", Port: 15434, Version: "17"}, &b); status(err) != 400 {
		t.Fatalf("restoring into an older version must be refused: %v", err)
	}
	if err := m.provision(ctx, instanceSpec{Name: "newer", Port: 15435, Version: "19"}, &b); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.record("newer"); r.Version != "19" {
		t.Fatalf("restore into a newer version: %q", r.Version)
	}
	if err := m.provision(ctx, instanceSpec{Name: "same", Port: 15436}, &b); err != nil {
		t.Fatal(err)
	}
	views, _ := m.instances(ctx)
	labels := map[string]string{}
	for _, v := range views {
		labels[v.Name] = v.EngineLabel
	}
	if labels["orders"] != "PostgreSQL 18" || labels["newer"] != "PostgreSQL 19" || labels["same"] != "PostgreSQL 18" {
		t.Fatalf("labels: %v", labels)
	}
}
