package main

// Listings in memory. The database list, the Backups page, backup storage
// and the scheduler read every backup on each refresh, and a library of
// thousands of backups would otherwise mean reading thousands of meta.json
// files every few seconds. Pages of backups and events are cut on the
// server, so a browser receives only the rows it shows.

import (
	"cmp"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Changes made outside the console are found by a full check this often.
var libraryRecheck = 30 * time.Second

type libraryIndex struct {
	mu      sync.Mutex
	changed atomic.Bool // the console changed a backup since the last scan
	dirMod  time.Time   // of the backups directory at the last scan
	scanned time.Time
	files   map[string]indexedBackup // by ID
	set     *library
}

type indexedBackup struct {
	mod  time.Time
	size int64
	b    backup
}

// One version of the library, shared by every reader: it never changes
// once built, so readers must not change it either.
type library struct {
	all    []backup            // newest first
	byName map[string][]backup // each newest first
	names  map[string][]string // databases with backups, by kind ("" for any)
}

// Note that the console changed a backup, so the next read looks again.
func (m *manager) backupsChanged() { m.lib.changed.Store(true) }

// The library now. A scan re-reads only the meta.json files that changed.
func (m *manager) library() (*library, error) {
	x := &m.lib
	x.mu.Lock()
	defer x.mu.Unlock()
	dir, err := os.Stat(m.backupDir())
	if err != nil {
		return nil, err
	}
	if x.set != nil && !x.changed.Load() && dir.ModTime().Equal(x.dirMod) && time.Since(x.scanned) < libraryRecheck {
		return x.set, nil
	}
	x.changed.Store(false) // a change during the scan marks it again
	scanned := time.Now()
	entries, err := os.ReadDir(m.backupDir())
	if err != nil {
		return nil, err
	}
	files := make(map[string]indexedBackup, len(entries))
	complete := true
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		info, err := os.Stat(filepath.Join(m.backupDir(), e.Name(), "meta.json"))
		if errors.Is(err, os.ErrNotExist) {
			complete = false // still being written
			continue
		}
		if err != nil {
			return nil, err
		}
		if f, ok := x.files[e.Name()]; ok && f.mod.Equal(info.ModTime()) && f.size == info.Size() {
			files[e.Name()] = f
			continue
		}
		b, err := m.backup(e.Name())
		var p *problem
		if errors.As(err, &p) && p.status == 404 {
			complete = false
			continue
		}
		if err != nil {
			return nil, err
		}
		files[e.Name()] = indexedBackup{info.ModTime(), info.Size(), b}
	}
	set := &library{all: make([]backup, 0, len(files)), byName: make(map[string][]backup), names: make(map[string][]string)}
	for _, f := range files {
		set.all = append(set.all, f.b)
	}
	slices.SortFunc(set.all, newestFirst)
	seen := map[string]bool{}
	for _, b := range set.all {
		set.byName[b.Name] = append(set.byName[b.Name], b)
		for _, kind := range []string{"", b.kindGroup()} {
			if key := kind + "/" + b.Name; !seen[key] {
				seen[key] = true
				set.names[kind] = append(set.names[kind], b.Name)
			}
		}
	}
	for _, list := range set.names {
		slices.Sort(list)
	}
	x.files, x.set, x.dirMod, x.scanned = files, set, dir.ModTime(), scanned
	if !complete {
		x.changed.Store(true)
	}
	return set, nil
}

func newestFirst(a, b backup) int {
	if c := b.Created.Compare(a.Created); c != 0 {
		return c
	}
	return strings.Compare(b.ID, a.ID)
}

// Snapshots and final snapshots are "manual"; scheduled backups "automated".
func (b backup) kindGroup() string {
	if b.Kind == "automated" {
		return "automated"
	}
	return "manual"
}

// What the Backups page calls it.
func (b backup) label() string {
	switch {
	case b.Final:
		return "Final snapshot"
	case b.pitrBase():
		return "Base backup"
	case b.Kind == "automated":
		return "Automated"
	}
	return "Manual"
}

// The backups of the database now using the name, newest first: not those
// of an earlier database deleted under the same name.
func (s *library) current(r record) []backup {
	list := s.byName[r.Name]
	end, _ := slices.BinarySearchFunc(list, r.Created, func(b backup, t time.Time) int { return t.Compare(b.Created) })
	for end < len(list) && !list[end].Created.Before(r.Created) {
		end++
	}
	return list[:end]
}

// ---------- pages

const (
	defaultPageSize = 50
	maxPageSize     = 500
)

type page struct {
	offset, limit int
}

// offset and limit from a query, or ok false when the request asks for the
// whole list (no limit), as it did before pages existed.
func readPage(q url.Values) (page, bool, error) {
	if !q.Has("limit") && !q.Has("offset") {
		return page{}, false, nil
	}
	p := page{limit: defaultPageSize}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxPageSize {
			return p, true, fail(400, "Set limit from 0 to 500.")
		}
		p.limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, true, fail(400, "Set offset to 0 or more.")
		}
		p.offset = n
	}
	return p, true, nil
}

func readSince(q url.Values) (time.Time, error) {
	v := q.Get("since")
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return t, fail(400, "Set since to an RFC 3339 time, such as 2026-01-02T03:04:05Z.")
	}
	return t, nil
}

// ---------- backups

type backupQuery struct {
	name, kind string    // kind: "manual" or "automated"
	since      time.Time // created at or after
	sort       string    // created, name, kind, size or expires
	asc        bool
	page
}

type kindTotal struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

type backupPage struct {
	Items  []backup `json:"items"`
	Total  int      `json:"total"` // matching every filter
	Bytes  int64    `json:"bytes"` // their size
	Offset int      `json:"offset"`
	Limit  int      `json:"limit"`
	// Databases with backups of the kind asked for, whichever database was
	// asked for, and the backups of each kind of the database asked for.
	Names []string             `json:"names"`
	Kinds map[string]kindTotal `json:"kinds"`
}

var backupSorts = []string{"created", "name", "kind", "size", "expires"}

func readBackupQuery(q url.Values) (backupQuery, bool, error) {
	bq := backupQuery{name: q.Get("name"), kind: q.Get("kind"), sort: cmp.Or(q.Get("sort"), "created")}
	var paged bool
	var err error
	if bq.page, paged, err = readPage(q); err != nil {
		return bq, paged, err
	}
	if bq.kind != "" && bq.kind != "manual" && bq.kind != "automated" {
		return bq, paged, fail(400, "Set kind to manual or automated.")
	}
	if !slices.Contains(backupSorts, bq.sort) {
		return bq, paged, fail(400, "Sort by created, name, kind, size or expires.")
	}
	switch q.Get("order") {
	case "asc":
		bq.asc = true
	case "", "desc":
	default:
		return bq, paged, fail(400, "Set order to asc or desc.")
	}
	bq.since, err = readSince(q)
	return bq, paged, err
}

func backupOrder(key string) func(a, b *backup) int {
	switch key {
	case "name":
		return func(a, b *backup) int { return strings.Compare(a.Name, b.Name) }
	case "kind":
		return func(a, b *backup) int { return strings.Compare(a.label(), b.label()) }
	case "size":
		return func(a, b *backup) int { return cmp.Compare(a.Bytes, b.Bytes) }
	case "expires": // backups that never expire come after every date
		return func(a, b *backup) int {
			if a.Expires == nil || b.Expires == nil {
				return cmp.Compare(boolInt(a.Expires == nil), boolInt(b.Expires == nil))
			}
			return a.Expires.Compare(*b.Expires)
		}
	}
	return func(a, b *backup) int { return a.Created.Compare(b.Created) }
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *library) query(q backupQuery) backupPage {
	p := backupPage{Items: []backup{}, Offset: q.offset, Limit: q.limit, Names: s.names[q.kind]}
	if p.Names == nil {
		p.Names = []string{}
	}
	source := s.all
	if q.name != "" {
		source = s.byName[q.name]
	}
	newest := q.sort == "created" && !q.asc // the order the library keeps
	var matched []*backup
	var manual, automated kindTotal
	for i := range source {
		b := &source[i]
		if !q.since.IsZero() && b.Created.Before(q.since) {
			continue
		}
		group, t := "manual", &manual
		if b.Kind == "automated" {
			group, t = "automated", &automated
		}
		t.Count++
		t.Bytes += b.Bytes
		if q.kind != "" && group != q.kind {
			continue
		}
		if newest && p.Total >= q.offset && p.Total < q.offset+q.limit {
			p.Items = append(p.Items, *b)
		}
		p.Total++
		p.Bytes += b.Bytes
		if !newest {
			matched = append(matched, b)
		}
	}
	p.Kinds = map[string]kindTotal{"manual": manual, "automated": automated}
	if newest {
		return p
	}
	order := backupOrder(q.sort)
	slices.SortFunc(matched, func(a, b *backup) int {
		c := order(a, b)
		if !q.asc {
			c = -c
		}
		if c != 0 {
			return c
		}
		return newestFirst(*a, *b)
	})
	for _, b := range matched[min(q.offset, len(matched)):min(q.offset+q.limit, len(matched))] {
		p.Items = append(p.Items, *b)
	}
	return p
}

// ---------- events

type eventQuery struct {
	name, result, text string
	since              time.Time // at or after
	page
}

type eventPage struct {
	Items  []event  `json:"items"`
	Total  int      `json:"total"` // matching every filter
	Count  int      `json:"count"` // every event kept
	Offset int      `json:"offset"`
	Limit  int      `json:"limit"`
	Names  []string `json:"names"` // databases and accounts in the history
}

func readEventQuery(q url.Values) (eventQuery, bool, error) {
	eq := eventQuery{name: q.Get("name"), result: q.Get("result"), text: strings.ToLower(strings.TrimSpace(q.Get("q")))}
	var paged bool
	var err error
	if eq.page, paged, err = readPage(q); err != nil {
		return eq, paged, err
	}
	if eq.result != "" && !slices.Contains([]string{"started", "completed", "failed"}, eq.result) {
		return eq, paged, fail(400, "Set result to started, completed or failed.")
	}
	eq.since, err = readSince(q)
	return eq, paged, err
}

func (e event) matches(q eventQuery) bool {
	if q.name != "" && e.Name != q.name || q.result != "" && e.Result != q.result || !q.since.IsZero() && e.At.Before(q.since) {
		return false
	}
	if q.text == "" {
		return true
	}
	return strings.Contains(strings.ToLower(e.Action+" "+e.Detail+" "+e.Name+" "+e.Actor), q.text)
}

func queryEvents(all []event, q eventQuery) eventPage {
	p := eventPage{Items: []event{}, Count: len(all), Offset: q.offset, Limit: q.limit, Names: []string{}}
	seen := map[string]bool{}
	for _, e := range all { // newest first
		if !seen[e.Name] {
			seen[e.Name] = true
			p.Names = append(p.Names, e.Name)
		}
		if !e.matches(q) {
			continue
		}
		if p.Total >= q.offset && p.Total < q.offset+q.limit {
			p.Items = append(p.Items, e)
		}
		p.Total++
	}
	slices.Sort(p.Names)
	return p
}
