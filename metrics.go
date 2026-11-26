package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultSampleInterval = 30 * time.Second
	historySpan           = 24 * time.Hour
	chartPoints           = 240
)

var containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

// One reading of a database container. Rates cover the time since the previous reading.
type point struct {
	At     int64   `json:"t"`      // Unix seconds
	CPU    float64 `json:"cpu"`    // percent of one CPU core
	Memory int64   `json:"memory"` // bytes, excluding reclaimable page cache
	Limit  int64   `json:"limit"`  // memory limit, or host memory when uncapped
	NetIn  float64 `json:"netIn"`  // bytes per second
	NetOut float64 `json:"netOut"`
	Read   float64 `json:"read"`
	Write  float64 `json:"write"`
	PIDs   int     `json:"pids"`
	// What the engine itself reports, when it answered.
	DB *dbPoint `json:"db,omitempty"`
}

// Cumulative counters from the previous reading of a container.
type counters struct {
	id                         string
	at                         time.Time
	cpu, system                uint64
	netIn, netOut, read, write uint64
	db                         *dbSample
}

// The fields OpenRDS uses from the Docker Engine stats API.
type rawStats struct {
	Read time.Time `json:"read"`
	CPU  struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System uint64 `json:"system_cpu_usage"`
		Online int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	PIDs struct {
		Current int `json:"current"`
	} `json:"pids_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	} `json:"networks"`
	Block struct {
		Bytes []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

type statsReader interface {
	stats(ctx context.Context, id string) (rawStats, error)
}

// The Docker CLI rounds stats to three significant digits, which turns
// long-running byte counters into coarse steps, so history reads the Engine API.
type engineAPI struct{ client *http.Client }

func dockerEngine() (statsReader, error) {
	socket := "/var/run/docker.sock"
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		path, ok := strings.CutPrefix(host, "unix://")
		if !ok {
			return nil, errors.New("metric history reads the Docker Engine API through a unix socket, and DOCKER_HOST is another kind of endpoint")
		}
		socket = path
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("metric history needs the Docker socket at %s", socket)
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	transport := &http.Transport{DialContext: dial, MaxIdleConns: 2, IdleConnTimeout: time.Minute}
	return engineAPI{&http.Client{Timeout: 20 * time.Second, Transport: transport}}, nil
}

func (e engineAPI) stats(ctx context.Context, id string) (rawStats, error) {
	var s rawStats
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/"+id+"/stats?stream=false&one-shot=true", nil)
	if err != nil {
		return s, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("container stats: HTTP %d", resp.StatusCode)
	}
	return s, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s)
}

type history struct {
	mu          sync.Mutex
	path        string
	interval    time.Duration
	unavailable string              // why history cannot be collected, if it cannot
	points      map[string][]point  // by Compose project, oldest first
	last        map[string]counters // baseline for the next rate
	lastError   string
}

func (h *history) init(path string, interval time.Duration) {
	h.path, h.interval = path, interval
	h.points = make(map[string][]point)
	h.last = make(map[string]counters)
}

func measure(id string, s rawStats) counters {
	c := counters{id: id, at: s.Read, cpu: s.CPU.Usage.Total, system: s.CPU.System}
	for _, n := range s.Networks {
		c.netIn += n.Rx
		c.netOut += n.Tx
	}
	for _, b := range s.Block.Bytes {
		switch strings.ToLower(b.Op) {
		case "read":
			c.read += b.Value
		case "write":
			c.write += b.Value
		}
	}
	return c
}

// Counters restart from zero with their container, which reads as no traffic.
func perSecond(now, before uint64, seconds float64) float64 {
	if now < before {
		return 0
	}
	return float64(now-before) / seconds
}

func (h *history) add(project, id string, s rawStats, db *dbSample) {
	cur := measure(id, s)
	cur.db = db
	h.mu.Lock()
	defer h.mu.Unlock()
	prev, ok := h.last[project]
	h.last[project] = cur
	seconds := cur.at.Sub(prev.at).Seconds()
	if !ok || prev.id != id || seconds <= 0 {
		return // the first reading of a container is only a baseline
	}
	p := point{At: cur.at.Unix(), Limit: int64(s.Memory.Limit), PIDs: s.PIDs.Current,
		NetIn: perSecond(cur.netIn, prev.netIn, seconds), NetOut: perSecond(cur.netOut, prev.netOut, seconds),
		Read: perSecond(cur.read, prev.read, seconds), Write: perSecond(cur.write, prev.write, seconds)}
	if cur.system > prev.system && cur.cpu >= prev.cpu {
		cpus := max(s.CPU.Online, 1)
		p.CPU = float64(cur.cpu-prev.cpu) / float64(cur.system-prev.system) * float64(cpus) * 100
	}
	// Match docker stats: page cache the kernel can reclaim is not counted as used.
	used := s.Memory.Usage
	cache, v1 := s.Memory.Stats["total_inactive_file"]
	if !v1 {
		cache = s.Memory.Stats["inactive_file"]
	}
	if cache < used {
		used -= cache
	}
	p.Memory = int64(used)
	if db != nil {
		p.DB = engineRates(prev.db, db)
	}
	h.points[project] = append(h.points[project], p)
}

// Drop readings older than the history span and series of removed instances.
func (h *history) prune(keep map[string]bool, now time.Time) {
	cutoff := now.Add(-historySpan).Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	for project, pts := range h.points {
		if !keep[project] {
			delete(h.points, project)
			continue
		}
		i := sort.Search(len(pts), func(i int) bool { return pts[i].At >= cutoff })
		h.points[project] = pts[i:]
	}
	for project := range h.last {
		if !keep[project] {
			delete(h.last, project)
		}
	}
}

func (h *history) forget(project string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.points, project)
	delete(h.last, project)
}

// The newest reading, if it is recent enough to describe the database now.
func (h *history) latest(project string, now time.Time) *point {
	h.mu.Lock()
	defer h.mu.Unlock()
	pts := h.points[project]
	if len(pts) == 0 || now.Unix()-pts[len(pts)-1].At > int64(3*h.interval/time.Second) {
		return nil
	}
	p := pts[len(pts)-1]
	return &p
}

// Readings within span, averaged into about chartPoints buckets.
func (h *history) series(project string, span time.Duration, now time.Time) []point {
	from := now.Add(-span).Unix()
	h.mu.Lock()
	pts := h.points[project]
	i := sort.Search(len(pts), func(i int) bool { return pts[i].At >= from })
	pts = slices.Clone(pts[i:])
	h.mu.Unlock()
	bucket := int64(span/time.Second) / chartPoints
	if bucket <= int64(h.interval/time.Second) {
		return pts
	}
	out := []point{}
	var sum point
	var n, key int64
	var db dbAverage
	flush := func() {
		if n == 0 {
			return
		}
		f := float64(n)
		out = append(out, point{At: sum.At / n, CPU: sum.CPU / f, Memory: sum.Memory / n, Limit: sum.Limit,
			NetIn: sum.NetIn / f, NetOut: sum.NetOut / f, Read: sum.Read / f, Write: sum.Write / f, PIDs: sum.PIDs, DB: db.point()})
	}
	for _, p := range pts {
		if k := p.At / bucket; n == 0 || k != key {
			flush()
			sum, n, key, db = point{}, 0, k, dbAverage{}
		}
		db.add(p.DB)
		sum.At += p.At
		sum.CPU += p.CPU
		sum.Memory += p.Memory
		sum.Limit = p.Limit
		sum.NetIn += p.NetIn
		sum.NetOut += p.NetOut
		sum.Read += p.Read
		sum.Write += p.Write
		sum.PIDs = max(sum.PIDs, p.PIDs)
		n++
	}
	flush()
	return out
}

// Engine readings averaged over a chart bucket. Readings the engine did not
// answer are left out rather than counted as zero.
type dbAverage struct {
	sum                 dbPoint
	n, active, hit, lag int
}

func (a *dbAverage) add(p *dbPoint) {
	if p == nil {
		return
	}
	a.n++
	a.sum.Conns += p.Conns
	a.sum.Ops += p.Ops
	a.sum.MaxConns = p.MaxConns
	if p.Active != nil {
		a.active++
		a.sum.Active = ptrAdd(a.sum.Active, *p.Active)
	}
	if p.Hit != nil {
		a.hit++
		a.sum.Hit = ptrAdd(a.sum.Hit, *p.Hit)
	}
	if p.Lag != nil {
		a.lag++
		a.sum.Lag = ptrAdd(a.sum.Lag, *p.Lag)
	}
}

func ptrAdd(p *float64, v float64) *float64 {
	if p == nil {
		return &v
	}
	sum := *p + v
	return &sum
}

func (a *dbAverage) point() *dbPoint {
	if a.n == 0 {
		return nil
	}
	f := float64(a.n)
	p := &dbPoint{Conns: a.sum.Conns / f, Ops: a.sum.Ops / f, MaxConns: a.sum.MaxConns}
	if a.active > 0 {
		v := *a.sum.Active / float64(a.active)
		p.Active = &v
	}
	if a.hit > 0 {
		v := *a.sum.Hit / float64(a.hit)
		p.Hit = &v
	}
	if a.lag > 0 {
		v := *a.sum.Lag / float64(a.lag)
		p.Lag = &v
	}
	return p
}

type historyView struct {
	Available bool    `json:"available"`
	Message   string  `json:"message,omitempty"`
	Interval  int     `json:"interval"` // seconds between readings
	Range     int     `json:"range"`    // seconds
	Now       int64   `json:"now"`      // server clock, so charts do not depend on the browser's
	Points    []point `json:"points"`
}

func (h *history) view(project string, span time.Duration, now time.Time) historyView {
	v := historyView{Available: h.unavailable == "", Interval: int(h.interval / time.Second), Range: int(span / time.Second), Now: now.Unix(), Points: []point{}}
	if !v.Available {
		v.Message = h.unavailable
		return v
	}
	if pts := h.series(project, span, now); pts != nil { // a database with no readings yet is [], not null
		v.Points = pts
	}
	return v
}

func (h *history) save() error {
	h.mu.Lock()
	data, err := json.Marshal(h.points)
	h.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFile(h.path, append(data, '\n'))
}

func (h *history) load(now time.Time) {
	saved := map[string][]point{}
	if err := readJSON(h.path, &saved); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("reading metric history: %v", err)
		}
		return
	}
	cutoff := now.Add(-historySpan).Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	for project, pts := range saved {
		i := sort.Search(len(pts), func(i int) bool { return pts[i].At >= cutoff })
		if i < len(pts) {
			h.points[project] = pts[i:]
		}
	}
}

// Log a repeated failure once instead of every interval.
func (h *history) report(err error) {
	text := ""
	if err != nil {
		text = err.Error()
	}
	h.mu.Lock()
	changed := text != h.lastError
	h.lastError = text
	h.mu.Unlock()
	if changed && text != "" {
		log.Printf("sampling metrics: %s", text)
	}
}

func (m *manager) sample(ctx context.Context, now time.Time) {
	records, err := m.records()
	if err != nil {
		m.history.report(err)
		return
	}
	known := make(map[string]bool, len(records))
	byProject := make(map[string]record, len(records))
	for _, r := range records {
		known[r.Project] = true
		byProject[r.Project] = r
	}
	m.history.prune(known, now)
	if m.engine == nil {
		return
	}
	var out bytes.Buffer
	err = m.docker.run(ctx, nil, nil, &out, "ps", "--filter", "label=io.openrds.database=true", "--filter", "status=running",
		"--format", `{{.ID}}|{{.Label "com.docker.compose.project"}}`)
	for _, line := range strings.Split(out.String(), "\n") {
		id, project, ok := strings.Cut(strings.TrimSpace(line), "|")
		if err != nil || !ok || !known[project] || !containerIDPattern.MatchString(id) {
			continue
		}
		s, statsErr := m.engine.stats(ctx, id)
		if statsErr != nil {
			err = statsErr
			continue
		}
		// Engine readings are best effort: a starting or overloaded database
		// still gets its container reading.
		r := byProject[project]
		db, _ := m.sampleEngine(ctx, r, id)
		if r.ReplicaOf != "" && !r.PendingRestore {
			st := m.replicationStatus(ctx, r, id)
			m.noteReplication(r.Name, st)
			if db != nil {
				db.lag = st.Lag
			}
		}
		m.history.add(project, id, s, db)
	}
	m.history.report(err)
}

// Sample every interval and keep a copy on disk so an update keeps the charts.
func (m *manager) sampleMetrics(ctx context.Context) {
	tick := time.NewTicker(m.history.interval)
	persist := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	defer persist.Stop()
	m.sample(ctx, time.Now().UTC())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			m.sample(ctx, now.UTC())
		case <-persist.C:
			if err := m.history.save(); err != nil {
				log.Printf("saving metric history: %v", err)
			}
		}
	}
}
