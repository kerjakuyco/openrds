package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What the console does on every refresh, with a large library: databases on
// hourly schedules, thousands of backups and a full event history. Run with
//
//	go test -run '^$' -bench Refresh -benchmem
func perfFixture(tb testing.TB, dbs, backups int) (*manager, http.Handler, string) {
	tb.Helper()
	projects := make([]string, dbs)
	docker := runFunc(func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
		switch args[0] {
		case "ps":
			if strings.Contains(strings.Join(args, " "), "io.openrds.database=true") {
				for _, p := range projects {
					fmt.Fprintf(out, "%s|running|Up 2 hours (healthy)\n", p)
				}
			}
		case "info":
			fmt.Fprint(out, `{"name":"perf","hostname":"perf","cpus":8,"memoryBytes":34359738368,"dockerVersion":"29.0.0","architecture":"x86_64"}`)
		}
		return nil
	})
	m, err := openManager(tb.TempDir(), "engines", docker)
	if err != nil {
		tb.Fatal(err)
	}
	m.space = plentyOfSpace
	now := time.Now().UTC()
	kinds := []string{"postgres", "mysql", "valkey"}
	for i := range dbs {
		r := record{instanceSpec: instanceSpec{Name: fmt.Sprintf("db-%02d", i), Engine: kinds[i%3], Port: 20000 + i,
			BackupRetentionDays: 7, BackupWindow: "03:00-03:30", BackupIntervalHours: 1, PITR: i%3 != 2,
			Tags: map[string]string{"team": "payments", "env": "production"}, Parameters: map[string]string{"max_connections": "200"}},
			Project: "openrds-db-" + randomHex(16), Password: randomHex(24), Created: now.Add(-60 * 24 * time.Hour), Layout: currentLayout}
		projects[i] = r.Project
		writeFixture(tb, m.recordPath(r.Name), r)
	}
	for i := range backups {
		db := i % dbs
		b := backup{ID: randomHex(16), Name: fmt.Sprintf("db-%02d", db), Engine: kinds[db%3], Version: "18",
			Created: now.Add(-time.Duration(i/dbs) * time.Hour), Bytes: 64 << 20, Kind: "automated",
			CPUs: 1, MemoryMiB: 1024, Parameters: map[string]string{"max_connections": "200"}}
		expires := b.Created.Add(7 * 24 * time.Hour)
		b.Expires = &expires
		if i%25 == 0 {
			b.Kind, b.Expires = "manual", nil
		}
		dir := filepath.Join(m.backupDir(), b.ID)
		if err := os.Mkdir(dir, 0700); err != nil {
			tb.Fatal(err)
		}
		writeFixture(tb, filepath.Join(dir, "meta.json"), b)
	}
	events := make([]event, activityLimit)
	for i := range events {
		events[i] = event{At: now.Add(-time.Duration(i) * time.Minute), Name: fmt.Sprintf("db-%02d", i%dbs),
			Action: "Automated backup", Result: []string{"completed", "started"}[i%2], Actor: "system"}
	}
	writeFixture(tb, filepath.Join(m.dir, "activity.json"), events)
	token, err := adminToken(m.dir)
	if err != nil {
		tb.Fatal(err)
	}
	people, err := openAccounts(filepath.Join(m.dir, "users.json"))
	if err != nil {
		tb.Fatal(err)
	}
	return m, handler(m, auth{token: token, accounts: people}), token
}

// Fixtures skip writeJSON's fsync: thousands of synced files stall the
// whole machine's disk, Docker included.
func writeFixture(tb testing.TB, path string, value any) {
	tb.Helper()
	data, err := json.Marshal(value)
	if err == nil {
		err = os.WriteFile(path, data, 0600)
	}
	if err != nil {
		tb.Fatal(err)
	}
}

func perfGet(tb testing.TB, h http.Handler, token, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		tb.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w
}

func BenchmarkRefresh(b *testing.B) {
	_, h, token := perfFixture(b, 40, 10000)
	paths := []string{"/api/instances", "/api/backups", "/api/backups?limit=50", "/api/backups?limit=0",
		"/api/backups?name=db-07&sort=size&limit=50", "/api/activity", "/api/activity?limit=20", "/api/storage", "/api/servers", "/api/host"}
	for _, gz := range []bool{false, true} {
		for _, path := range paths {
			name := strings.TrimPrefix(path, "/api/")
			header := []string{}
			if gz {
				name, header = name+"+gzip", []string{"Accept-Encoding", "gzip"}
			}
			b.Run(name, func(b *testing.B) {
				perfGet(b, h, token, path, header...) // the first read builds the library index
				var size int
				for b.Loop() {
					size = perfGet(b, h, token, path, header...).Body.Len()
				}
				b.ReportMetric(float64(size), "resp-bytes")
			})
		}
	}
}
