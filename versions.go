package main

// Engine versions: a curated list per engine, from stable to latest. A
// database keeps the version it was created with, so nothing upgrades by
// surprise. To move to a newer version, restore a snapshot into a new
// database: backups are logical, so newer versions read older ones.

import (
	"fmt"
	"strconv"
	"strings"
)

type engineVersion struct {
	Version string `json:"version"`
	Status  string `json:"status,omitempty"` // latest, stable, or empty for older versions
	LTS     bool   `json:"lts,omitempty"`
}

type versionCatalog struct {
	image  string          // image reference with the version as %s
	legacy string          // the version of databases created before versions existed
	list   []engineVersion // newest first; the stable one is the default
}

var engineVersions = map[string]versionCatalog{
	"postgres": {image: "postgres:%s-alpine", legacy: "17", list: []engineVersion{
		{"19", "latest", false}, {"18", "stable", false}, {"17", "", false}, {"16", "", false}, {"15", "", false},
	}},
	"mysql": {image: "mysql:%s", legacy: "8.4", list: []engineVersion{
		{"9.7", "latest", true}, {"8.4", "stable", true},
	}},
	"valkey": {image: "valkey/valkey:%s-alpine", legacy: "9.1", list: []engineVersion{
		{"9.2", "latest", false}, {"9.1", "stable", false}, {"9.0", "", false}, {"8.1", "", false}, {"8.0", "", false}, {"7.2", "", false},
	}},
}

func defaultVersion(engine string) string {
	for _, v := range engineVersions[engine].list {
		if v.Status == "stable" {
			return v.Version
		}
	}
	return engineVersions[engine].legacy
}

// Only listed versions can be chosen for a new database.
func offeredVersion(engine, version string) bool {
	for _, v := range engineVersions[engine].list {
		if v.Version == version {
			return true
		}
	}
	return false
}

// Compare dotted versions numerically: 9.10 is newer than 9.7.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (r record) version() string {
	if r.Version != "" {
		return r.Version
	}
	return engineVersions[r.Engine].legacy
}

func (r record) image() string {
	return fmt.Sprintf(engineVersions[r.Engine].image, r.version())
}

// Where the data volume is mounted. From PostgreSQL 18 the official image
// keeps its data in a versioned folder under /var/lib/postgresql.
func (r record) dataMount() string {
	if r.Engine == "postgres" && compareVersions(r.version(), "18") >= 0 {
		return "/var/lib/postgresql"
	}
	return engines[r.Engine].dataDir
}

func (b backup) version() string {
	if b.Version != "" {
		return b.Version
	}
	return engineVersions[b.Engine].legacy
}

func (r record) engineLabel() string {
	return engines[r.Engine].title + " " + r.version()
}
