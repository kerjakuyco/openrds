package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func addBackup(t *testing.T, m *manager, b backup) backup {
	t.Helper()
	if b.ID == "" {
		b.ID = randomHex(16)
	}
	dir := filepath.Join(m.backupDir(), b.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("dump"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func ids(list []backup) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.ID
	}
	return out
}

func TestTheLibraryFollowsEveryChange(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	now := time.Now().UTC().Truncate(time.Second)
	older := addBackup(t, m, backup{Name: "orders", Engine: "postgres", Created: now.Add(-time.Hour), Kind: "automated", Bytes: 1})
	if list, err := m.backups(); err != nil || !slices.Equal(ids(list), []string{older.ID}) {
		t.Fatalf("first read: %v %v", ids(list), err)
	}
	newer := addBackup(t, m, backup{Name: "orders", Engine: "postgres", Created: now, Kind: "manual", Bytes: 2})
	if list, _ := m.backups(); !slices.Equal(ids(list), []string{newer.ID, older.ID}) {
		t.Fatalf("a new backup is found, newest first: %v", ids(list))
	}
	if _, err := m.keepBackup(t.Context(), older.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.backups(); list[1].Kind != "manual" || list[1].Expires != nil {
		t.Fatalf("a kept backup reads as a snapshot at once: %+v", list[1])
	}
	// A change made outside the console shows at the next full check.
	edited := newer
	edited.Note = "edited by hand"
	if err := writeJSON(filepath.Join(m.backupDir(), newer.ID, "meta.json"), edited); err != nil {
		t.Fatal(err)
	}
	recheck := libraryRecheck
	libraryRecheck = 0
	list, _ := m.backups()
	libraryRecheck = recheck
	if list[0].Note != "edited by hand" {
		t.Fatalf("an edited backup: %+v", list[0])
	}
	if err := m.deleteBackup(t.Context(), newer.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.backups(); !slices.Equal(ids(list), []string{older.ID}) {
		t.Fatalf("a deleted backup is gone at once: %v", ids(list))
	}
	// A backup whose metadata is still being written appears once it lands.
	late := backup{ID: randomHex(16), Name: "orders", Engine: "postgres", Created: now.Add(time.Minute), Kind: "manual"}
	if err := os.Mkdir(filepath.Join(m.backupDir(), late.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.backups(); len(list) != 1 {
		t.Fatalf("a backup without metadata is not listed: %v", ids(list))
	}
	if err := writeJSON(filepath.Join(m.backupDir(), late.ID, "meta.json"), late); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.backups(); !slices.Equal(ids(list), []string{late.ID, older.ID}) {
		t.Fatalf("its metadata landed: %v", ids(list))
	}
	// Callers get their own list.
	list, _ = m.backups()
	list[0].Name = "changed"
	if again, _ := m.backups(); again[0].Name != "orders" {
		t.Fatal("a caller changed the library")
	}
}

func getJSON(t *testing.T, h http.Handler, path string, into any) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == 200 && into != nil {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	return w.Code
}

func TestBackupPages(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	now := time.Now().UTC().Truncate(time.Second)
	created := now.Add(-48 * time.Hour)
	if err := writeJSON(m.recordPath("orders"), record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, BackupRetentionDays: 7},
		Project: "openrds-db-orders", Created: created}); err != nil {
		t.Fatal(err)
	}
	var made []backup
	for i := range 12 {
		b := backup{Name: []string{"orders", "users", "carts"}[i%3], Engine: "postgres", Created: now.Add(-time.Duration(i) * time.Hour), Bytes: int64(100 + i), Kind: "automated"}
		expires := b.Created.Add(time.Duration(24+2*i) * time.Hour)
		b.Expires = &expires
		if i%4 == 0 {
			b.Kind, b.Expires = "manual", nil
		}
		made = append(made, addBackup(t, m, b))
	}
	// A backup of an earlier database named orders, deleted before this one.
	addBackup(t, m, backup{Name: "orders", Engine: "postgres", Created: created.Add(-time.Hour), Bytes: 1, Kind: "automated"})
	h := handler(m, testAuth(t, m, "test-token"))

	var all []backup
	if getJSON(t, h, "/api/backups", &all); len(all) != 13 || all[0].ID != made[0].ID {
		t.Fatalf("without a limit, every backup as before: %d", len(all))
	}
	var p backupPage
	getJSON(t, h, "/api/backups?limit=5", &p)
	if len(p.Items) != 5 || p.Total != 13 || p.Items[0].ID != made[0].ID || !slices.Equal(p.Names, []string{"carts", "orders", "users"}) ||
		p.Kinds["manual"].Count != 3 || p.Kinds["automated"].Count != 10 {
		t.Fatalf("first page: %d items, %+v", len(p.Items), p)
	}
	getJSON(t, h, "/api/backups?limit=5&offset=10", &p)
	if len(p.Items) != 3 || p.Offset != 10 || p.Items[0].ID != made[10].ID {
		t.Fatalf("last page: %d items from %d", len(p.Items), p.Offset)
	}
	getJSON(t, h, "/api/backups?offset=0", &p)
	if p.Limit != defaultPageSize || len(p.Items) != 13 {
		t.Fatalf("an offset alone pages with the default size: %d", p.Limit)
	}
	getJSON(t, h, "/api/backups?name=orders&kind=automated&limit=50", &p)
	if p.Total != 4 || p.Kinds["manual"].Count != 1 || !slices.Equal(p.Names, []string{"carts", "orders", "users"}) {
		t.Fatalf("filtered: %+v", p)
	}
	since := created.Format(time.RFC3339)
	getJSON(t, h, "/api/backups?name=orders&since="+since+"&limit=0", &p)
	if len(p.Items) != 0 || p.Total != 4 || p.Bytes != 100+103+106+109 {
		t.Fatalf("the current database's backups: %+v", p)
	}
	getJSON(t, h, "/api/backups?sort=size&order=asc&limit=3", &p)
	if p.Items[0].Bytes != 1 || p.Items[1].Bytes != 100 || p.Items[2].Bytes != 101 {
		t.Fatalf("smallest first: %v", p.Items)
	}
	getJSON(t, h, "/api/backups?sort=expires&order=asc&limit=50", &p)
	if last := p.Items[len(p.Items)-1]; p.Items[0].Expires == nil || last.Expires != nil || !p.Items[0].Expires.Before(*p.Items[1].Expires) {
		t.Fatalf("soonest to expire first, never last: %v", p.Items)
	}
	getJSON(t, h, "/api/backups?sort=kind&limit=50", &p)
	if p.Items[0].label() != "Manual" || p.Items[len(p.Items)-1].label() != "Automated" {
		t.Fatalf("by type: %s … %s", p.Items[0].label(), p.Items[len(p.Items)-1].label())
	}
	for _, bad := range []string{"limit=501", "limit=x", "offset=-1", "kind=final", "sort=id", "order=up", "since=yesterday"} {
		if code := getJSON(t, h, "/api/backups?"+bad, nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	var one backup
	if getJSON(t, h, "/api/backups/"+made[3].ID, &one); one.ID != made[3].ID || one.Name != made[3].Name {
		t.Fatalf("one backup: %+v", one)
	}
	if code := getJSON(t, h, "/api/backups/"+randomHex(16), nil); code != 404 {
		t.Fatalf("an unknown backup: %d", code)
	}

	list, err := m.instances(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	if d := list[0]; d.Automated != 3 || d.LatestBackup == nil || !d.LatestBackup.Equal(made[0].Created) {
		t.Fatalf("orders keeps 3 automated backups of its own: %d, latest %v", d.Automated, d.LatestBackup)
	}
}

func TestEventPages(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	for i := range 30 {
		m.logEvent(t.Context(), event{Name: []string{"orders", "users"}[i%2], Action: fmt.Sprintf("Action %d", i), Result: []string{"completed", "failed", "started"}[i%3]})
	}
	h := handler(m, testAuth(t, m, "test-token"))
	var all []event
	if getJSON(t, h, "/api/activity", &all); len(all) != 30 || all[0].Action != "Action 29" {
		t.Fatalf("without a limit, every event, newest first: %d", len(all))
	}
	var p eventPage
	getJSON(t, h, "/api/activity?limit=10", &p)
	if len(p.Items) != 10 || p.Total != 30 || p.Count != 30 || !slices.Equal(p.Names, []string{"orders", "users"}) {
		t.Fatalf("first page: %+v", p)
	}
	getJSON(t, h, "/api/activity?result=failed&limit=0", &p)
	if p.Total != 10 || len(p.Items) != 0 {
		t.Fatalf("a count of failures: %+v", p)
	}
	getJSON(t, h, "/api/activity?name=orders&q=ACTION+2&limit=50", &p)
	if p.Total != 6 || p.Items[0].Action != "Action 28" {
		t.Fatalf("search: %+v", p)
	}
	since := all[4].At
	want := 0
	for _, e := range all {
		if !e.At.Before(since) {
			want++
		}
	}
	getJSON(t, h, "/api/activity?limit=50&since="+since.Format(time.RFC3339Nano), &p)
	if p.Total != want {
		t.Fatalf("since %v: %d, want %d", since, p.Total, want)
	}
	for _, bad := range []string{"result=ok", "limit=-1", "since=now"} {
		if code := getJSON(t, h, "/api/activity?"+bad, nil); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// The history is kept on disk for the next start.
	again, err := openManager(m.dir, "engines", newFakeDocker())
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := again.readActivity(); len(items) != 30 || items[0].Action != "Action 29" {
		t.Fatalf("after a restart: %d", len(items))
	}
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheConsoleIsCompressedAndCached(t *testing.T) {
	m := testManager(t, newFakeDocker().run)
	h := handler(m, testAuth(t, m, "test-token"))
	get := func(path string, header ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	page, _ := assets.ReadFile("web/index.html")
	w := get("/", "Accept-Encoding", "gzip, deflate, br")
	etag := w.Header().Get("ETag")
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Cache-Control") != "no-cache" || etag == "" ||
		w.Header().Get("Vary") != "Accept-Encoding" || !bytes.Equal(gunzip(t, w.Body.Bytes()), page) {
		t.Fatalf("the page: %d %v", w.Code, w.Header())
	}
	if w := get("/", "Accept-Encoding", "gzip", "If-None-Match", etag); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("a reload revalidates: %d", w.Code)
	}
	if w := get("/"); w.Header().Get("Content-Encoding") != "" || w.Header().Get("ETag") == etag || !bytes.Equal(w.Body.Bytes(), page) {
		t.Fatalf("without gzip: %v", w.Header())
	}
	if w := get("/app.mjs", "Accept-Encoding", "gzip"); !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("scripts: %v", w.Header())
	}
	if w := get("/fonts/source-sans-3.woff2", "Accept-Encoding", "gzip"); w.Code != 200 || w.Header().Get("Content-Encoding") != "" || w.Header().Get("Content-Type") != "font/woff2" {
		t.Fatalf("fonts are already compressed: %v", w.Header())
	}
	if w := get("/index.html"); w.Code != 301 {
		t.Fatalf("/index.html: %d", w.Code)
	}
	if w := get("/missing.mjs"); w.Code != 404 {
		t.Fatalf("a missing file: %d", w.Code)
	}

	now := time.Now().UTC()
	var first backup
	for i := range 40 {
		b := addBackup(t, m, backup{Name: "orders", Engine: "postgres", Created: now.Add(-time.Duration(i) * time.Minute), Kind: "manual", Bytes: 4})
		if i == 0 {
			first = b
		}
	}
	plain := get("/api/backups?limit=40")
	zipped := get("/api/backups?limit=40", "Accept-Encoding", "gzip")
	if zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Header().Get("Cache-Control") != "no-store" ||
		!bytes.Equal(gunzip(t, zipped.Body.Bytes()), plain.Body.Bytes()) || zipped.Body.Len()*3 > plain.Body.Len() {
		t.Fatalf("a large reply is compressed: %d of %d bytes, %v", zipped.Body.Len(), plain.Body.Len(), zipped.Header())
	}
	if w := get("/api/storage", "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("a small reply is not: %v", w.Header())
	}
	if w := get("/api/backups/"+first.ID+"/download", "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "dump" {
		t.Fatalf("a download is sent as it is: %v %q", w.Header(), w.Body.String())
	}
}

// Reads docker info from the Engine API and counts how often.
type infoEngine struct {
	fakeEngine
	calls  *int
	answer serverInfo
}

func (e infoEngine) info(context.Context) (serverInfo, error) {
	*e.calls++
	return e.answer, nil
}

func TestTheConsolesDockerAnswersThroughTheEngineAPI(t *testing.T) {
	cli := 0
	m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, _ io.Writer, args ...string) error {
		if args[0] == "info" {
			cli++
		}
		return nil
	})
	calls := 0
	m.engine = infoEngine{fakeEngine{}, &calls, serverInfo{Name: "vps", CPUs: 4, MemoryBytes: 8 << 30, Version: "29.1.0", Arch: "x86_64"}}
	host, err := m.host(t.Context())
	if err != nil || host.Name != "vps" || host.CPUs != 4 || host.MemoryBytes != 8<<30 || host.Version != "29.1.0" {
		t.Fatalf("host: %+v %v", host, err)
	}
	views, err := m.serverViews(t.Context())
	if err != nil || views[0].Info == nil || views[0].Info.Arch != "x86_64" || !views[0].Reachable {
		t.Fatalf("servers: %+v %v", views, err)
	}
	if calls != 1 || cli != 0 {
		t.Fatalf("one Engine API read serves both, without the CLI: %d API, %d CLI", calls, cli)
	}
}

// The Engine API client against a stand-in for the Docker daemon: refreshes
// list containers and read docker info without starting the docker CLI.
func TestTheEngineAPIAnswersLikeTheCLI(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch r.URL.Path {
		case "/containers/json":
			var filters map[string][]string
			json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
			if !slices.Equal(filters["label"], []string{"io.openrds.database=true"}) {
				http.Error(w, "unexpected filters", 400)
				return
			}
			list := `[{"Id":"` + strings.Repeat("a", 64) + `","Labels":{"com.docker.compose.project":"openrds-db-orders"},"State":"running","Status":"Up 2 hours (healthy)"}`
			if r.URL.Query().Get("all") == "true" {
				list += `,{"Id":"` + strings.Repeat("b", 64) + `","Labels":{"com.docker.compose.project":"openrds-db-carts"},"State":"exited","Status":"Exited (0) 1 hour ago"}`
			}
			fmt.Fprint(w, list+"]")
		case "/info":
			fmt.Fprint(w, `{"Name":"vps","NCPU":8,"MemTotal":17179869184,"ServerVersion":"29.1.0","Architecture":"aarch64","Plugins":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", daemon.Listener.Addr().String())
	}
	cli := 0
	m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, _ io.Writer, args ...string) error {
		cli++
		return nil
	})
	m.engine = engineAPI{&http.Client{Transport: &http.Transport{DialContext: dial}}}
	states, err := m.states(t.Context())
	if err != nil || states["openrds-db-orders"] != "running" || states["openrds-db-carts"] != "stopped" {
		t.Fatalf("states: %v %v", states, err)
	}
	info, err := m.dockerInfo(t.Context(), localServer)
	if err != nil || info.Name != "vps" || info.CPUs != 8 || info.MemoryBytes != 16<<30 || info.Version != "29.1.0" || info.Arch != "aarch64" {
		t.Fatalf("info: %+v %v", info, err)
	}
	lister, _ := m.localContainers(localServer)
	running, err := lister.containers(t.Context(), "io.openrds.database=true", true)
	if err != nil || len(running) != 1 || running[0].ID != strings.Repeat("a", 64) || running[0].Project != "openrds-db-orders" {
		t.Fatalf("running: %+v %v", running, err)
	}
	if cli != 0 {
		t.Fatalf("the docker CLI ran %d times", cli)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(asked[0], "all=true") || strings.Contains(asked[2], "all=true") || !strings.Contains(asked[2], "running") {
		t.Fatalf("queries: %v", asked)
	}
}
