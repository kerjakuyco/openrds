package main

// Off-server copies of backups in S3-compatible object storage (Amazon S3,
// Cloudflare R2, Backblaze B2, Wasabi, MinIO). Requests are signed with AWS
// Signature Version 4 using only the standard library. Every backup in the
// library gets a copy; copies follow the library's deletes and expiry, and a
// new server pointed at the same bucket can import them.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,200}$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	keyPattern    = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)
	slashes       = regexp.MustCompile(`/+`)
	emptyHash     = hex.EncodeToString(sha256.New().Sum(nil))
	// Larger files go up in parts; variables so tests can use small files.
	multipartAbove int64 = 64 << 20
	minPartSize    int64 = 16 << 20
)

const offsiteRetry = 10 * time.Minute // before trying a failed copy again

type offsiteConfig struct {
	Enabled   bool   `json:"enabled"`
	Provider  string `json:"provider,omitempty"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"accessKeyId"`
	SecretKey string `json:"secretAccessKey,omitempty"`
	PathStyle bool   `json:"pathStyle"`
}

// The state of a backup's copy in storage, kept in its meta.json.
type offsiteCopy struct {
	Status string    `json:"status"` // copied or failed
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

type offsiteState struct {
	mu   sync.Mutex
	run  sync.Mutex // one sweep at a time
	last time.Time
	ctx  context.Context // outlives requests that start a sweep
}

func (m *manager) offsitePath() string { return filepath.Join(m.dir, "offsite.json") }

func (m *manager) loadOffsite() (offsiteConfig, error) {
	var cfg offsiteConfig
	err := readJSON(m.offsitePath(), &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return offsiteConfig{}, nil
	}
	return cfg, err
}

// Settings in canonical form; the secret is kept when the request leaves it empty.
func (m *manager) checkOffsite(in offsiteConfig) (offsiteConfig, error) {
	cfg := in
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return cfg, fail(400, "Use an endpoint URL such as https://s3.us-east-1.amazonaws.com.")
	}
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if !regionPattern.MatchString(cfg.Region) {
		return cfg, fail(400, "Use a region such as us-east-1, or auto for Cloudflare R2.")
	}
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	if !bucketPattern.MatchString(cfg.Bucket) {
		return cfg, fail(400, "Use the bucket’s name: 3–63 lowercase letters, digits, dots and hyphens.")
	}
	cfg.Prefix = strings.Trim(slashes.ReplaceAllString(strings.TrimSpace(cfg.Prefix), "/"), "/")
	if !prefixPattern.MatchString(cfg.Prefix) || slices.Contains(strings.Split(cfg.Prefix, "/"), "..") {
		return cfg, fail(400, "Use a folder made of letters, digits and . _ / -.")
	}
	if cfg.Prefix != "" {
		cfg.Prefix += "/"
	}
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	if !keyPattern.MatchString(cfg.AccessKey) {
		return cfg, fail(400, "Enter the access key ID.")
	}
	if cfg.SecretKey == "" {
		current, err := m.loadOffsite()
		if err != nil {
			return cfg, err
		}
		cfg.SecretKey = current.SecretKey
	}
	if !keyPattern.MatchString(cfg.SecretKey) {
		return cfg, fail(400, "Enter the secret access key.")
	}
	if len(cfg.Provider) > 20 {
		cfg.Provider = ""
	}
	return cfg, nil
}

// Write, read back and delete a small object, so settings are only saved
// when they work.
func testOffsite(ctx context.Context, cfg offsiteConfig) error {
	c, err := newS3(cfg)
	if err != nil {
		return err
	}
	// The request no longer ends with the client, so an unresponsive endpoint needs its own limit.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	key := cfg.Prefix + ".openrds-test-" + randomHex(6)
	want := []byte("openrds storage test\n")
	if err := c.putBytes(ctx, key, want); err != nil {
		return fail(502, "Could not write to the bucket: "+err.Error())
	}
	got, err := c.getBytes(ctx, key, 1<<10)
	if err == nil && !bytes.Equal(got, want) {
		err = errors.New("the object read back differs")
	}
	if err != nil {
		return fail(502, "Could not read from the bucket: "+err.Error())
	}
	if err := c.delete(ctx, key); err != nil {
		return fail(502, "Could not delete from the bucket: "+err.Error())
	}
	return nil
}

func (m *manager) saveOffsite(ctx context.Context, in offsiteConfig) error {
	cfg, err := m.checkOffsite(in)
	if err != nil {
		return err
	}
	if err := testOffsite(ctx, cfg); err != nil {
		return err
	}
	cfg.Enabled = true
	if err := writeJSON(m.offsitePath(), cfg); err != nil {
		return err
	}
	m.logEvent(ctx, event{Name: "Backup storage", Kind: "storage", Action: "Connect backup storage", Result: "completed", Detail: "s3://" + cfg.Bucket + "/" + cfg.Prefix})
	go m.syncOffsite()
	return nil
}

// Stop copying and forget the credentials. Copies already in storage stay.
func (m *manager) disableOffsite(ctx context.Context) error {
	if err := os.Remove(m.offsitePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.logEvent(ctx, event{Name: "Backup storage", Kind: "storage", Action: "Disconnect backup storage", Result: "completed"})
	return nil
}

type offsiteView struct {
	Enabled   bool       `json:"enabled"`
	Provider  string     `json:"provider,omitempty"`
	Endpoint  string     `json:"endpoint,omitempty"`
	Region    string     `json:"region,omitempty"`
	Bucket    string     `json:"bucket,omitempty"`
	Prefix    string     `json:"prefix,omitempty"`
	AccessKey string     `json:"accessKeyId,omitempty"`
	PathStyle bool       `json:"pathStyle"`
	Copied    int        `json:"copied"`
	Pending   int        `json:"pending"`
	Failed    int        `json:"failed"`
	LastSync  *time.Time `json:"lastSyncAt,omitempty"`
	LastError string     `json:"lastError,omitempty"`
}

// What everyone may see about storage; the access key ID only for admins,
// the secret never.
func (m *manager) offsiteStatus(admin bool) (offsiteView, error) {
	cfg, err := m.loadOffsite()
	if err != nil || !cfg.Enabled {
		return offsiteView{}, err
	}
	v := offsiteView{Enabled: true, Provider: cfg.Provider, Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, Prefix: cfg.Prefix, PathStyle: cfg.PathStyle}
	if admin {
		v.AccessKey = cfg.AccessKey
	}
	list, err := m.backups()
	if err != nil {
		return v, err
	}
	var latest time.Time
	for _, b := range list {
		switch {
		case b.Offsite != nil && b.Offsite.Status == "copied":
			v.Copied++
		case b.Offsite != nil && b.Offsite.Status == "failed":
			v.Failed++
			if b.Offsite.At.After(latest) { // the reason stays until the copy succeeds
				latest, v.LastError = b.Offsite.At, b.Offsite.Error
			}
		default:
			v.Pending++
		}
	}
	m.offsite.mu.Lock()
	if !m.offsite.last.IsZero() {
		t := m.offsite.last
		v.LastSync = &t
	}
	m.offsite.mu.Unlock()
	return v, nil
}

func (m *manager) backgroundContext() context.Context {
	if m.offsite.ctx != nil {
		return m.offsite.ctx
	}
	return context.Background()
}

// Copy every backup that has no copy yet. Failed copies are retried after a
// pause. Runs after each backup and from the maintenance loop.
func (m *manager) syncOffsite() {
	if !m.offsite.run.TryLock() {
		return
	}
	defer m.offsite.run.Unlock()
	ctx := m.backgroundContext()
	cfg, err := m.loadOffsite()
	if err != nil || !cfg.Enabled {
		return
	}
	c, err := newS3(cfg)
	if err != nil {
		return
	}
	list, err := m.backups()
	if err != nil {
		return
	}
	for _, b := range slices.Backward(list) { // oldest first
		if ctx.Err() != nil {
			return
		}
		if b.Offsite != nil && (b.Offsite.Status == "copied" || time.Since(b.Offsite.At) < offsiteRetry) {
			continue
		}
		err := m.copyBackup(ctx, c, b)
		state := offsiteCopy{Status: "copied", At: time.Now().UTC()}
		if err != nil {
			state = offsiteCopy{Status: "failed", At: time.Now().UTC(), Error: err.Error()}
		}
		if m.markOffsite(b.ID, state) {
			detail := "s3://" + cfg.Bucket + "/" + cfg.Prefix + b.ID
			if err != nil {
				m.logEvent(ctx, event{Name: b.Name, Action: "Copy backup to storage", Result: "failed", Detail: err.Error()})
			} else {
				m.logEvent(ctx, event{Name: b.Name, Action: "Copy backup to storage", Result: "completed", Detail: detail})
			}
		}
	}
	m.offsite.mu.Lock()
	m.offsite.last = time.Now().UTC()
	m.offsite.mu.Unlock()
}

// Data first, then metadata, so a listed backup is always complete.
func (m *manager) copyBackup(ctx context.Context, c *s3, b backup) error {
	f, err := m.openDump(b.ID)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := c.putFile(ctx, c.cfg.Prefix+b.ID+"/data", f, b.Bytes); err != nil {
		return err
	}
	meta := b
	meta.Offsite = nil
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return c.putBytes(ctx, c.cfg.Prefix+b.ID+"/meta.json", data)
}

// Record a copy's state, unless the backup was deleted meanwhile.
func (m *manager) markOffsite(id string, state offsiteCopy) bool {
	m.backupMu.Lock()
	defer m.backupMu.Unlock()
	b, err := m.backup(id)
	if err != nil {
		return false
	}
	b.Offsite = &state
	if err := writeJSON(filepath.Join(m.backupDir(), id, "meta.json"), b); err != nil {
		log.Printf("recording the storage copy of %s: %v", id, err)
		return false
	}
	return true
}

// Copies follow the library: deleting or expiring a backup deletes its copy.
func (m *manager) removeOffsite(b backup) {
	if b.Offsite == nil || b.Offsite.Status != "copied" {
		return
	}
	cfg, err := m.loadOffsite()
	if err != nil || !cfg.Enabled {
		return
	}
	c, err := newS3(cfg)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.backgroundContext(), 2*time.Minute)
	defer cancel()
	for _, key := range []string{cfg.Prefix + b.ID + "/meta.json", cfg.Prefix + b.ID + "/data"} {
		if err := c.delete(ctx, key); err != nil {
			m.logEvent(ctx, event{Name: b.Name, Action: "Delete storage copy", Result: "failed", Detail: err.Error()})
			return
		}
	}
}

type remoteBackup struct {
	backup
	Local bool `json:"local"` // also in this server's library
}

// Backups in storage, newest first, including those from other servers.
func (m *manager) remoteBackups(ctx context.Context) ([]remoteBackup, error) {
	cfg, err := m.loadOffsite()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fail(409, "Connect backup storage first.")
	}
	c, err := newS3(cfg)
	if err != nil {
		return nil, err
	}
	objects, err := c.list(ctx, cfg.Prefix)
	if err != nil {
		return nil, fail(502, "Could not list the bucket: "+err.Error())
	}
	out := []remoteBackup{}
	for _, o := range objects {
		id, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, cfg.Prefix), "/meta.json")
		if !ok || !idPattern.MatchString(id) || len(out) >= 1000 {
			continue
		}
		data, err := c.getBytes(ctx, o.Key, 64<<10)
		if err != nil {
			continue
		}
		var b backup
		if json.Unmarshal(data, &b) != nil || b.ID != id {
			continue
		}
		_, statErr := os.Stat(filepath.Join(m.backupDir(), id, "meta.json"))
		b.Offsite = nil
		out = append(out, remoteBackup{b, statErr == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Bring a backup from storage into this server's library as a snapshot, so
// it can be restored here.
func (m *manager) importBackup(ctx context.Context, id string) (b backup, err error) {
	if !idPattern.MatchString(id) {
		return b, fail(404, "Backup not found.")
	}
	cfg, err := m.loadOffsite()
	if err != nil {
		return b, err
	}
	if !cfg.Enabled {
		return b, fail(409, "Connect backup storage first.")
	}
	if _, err := os.Stat(filepath.Join(m.backupDir(), id)); err == nil {
		return b, fail(409, "This backup is already on this server.")
	}
	c, err := newS3(cfg)
	if err != nil {
		return b, err
	}
	data, err := c.getBytes(ctx, cfg.Prefix+id+"/meta.json", 64<<10)
	if err != nil {
		return b, fail(502, "Could not read the backup from storage: "+err.Error())
	}
	if json.Unmarshal(data, &b) != nil || b.ID != id || !namePattern.MatchString(b.Name) {
		return b, fail(502, "The backup’s metadata in storage is not valid.")
	}
	if _, ok := engines[b.Engine]; !ok {
		return b, fail(502, "The backup is for an engine this server does not run.")
	}
	if err := m.checkBackupSpace(b.Bytes); err != nil {
		return b, err
	}
	finish := m.trackDetail(ctx, b.Name, "Import backup from storage", id)
	defer func() { finish(err) }()
	stage, err := os.MkdirTemp(m.backupDir(), ".importing-")
	if err != nil {
		return b, err
	}
	defer os.RemoveAll(stage)
	body, size, err := c.get(ctx, cfg.Prefix+id+"/data")
	if err != nil {
		return b, fail(502, "Could not download the backup: "+err.Error())
	}
	defer body.Close()
	f, err := os.OpenFile(filepath.Join(stage, "data"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return b, err
	}
	guard := m.guardSpace(f)
	n, err := io.Copy(guard, body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if guard.err != nil {
		return b, guard.err
	}
	if err != nil {
		return b, fail(502, "Could not download the backup: "+err.Error())
	}
	if n != b.Bytes || (size >= 0 && size != n) {
		return b, fail(502, fmt.Sprintf("The download is incomplete: %d of %d bytes.", n, b.Bytes))
	}
	// An imported backup is a snapshot here: it must not expire on arrival.
	b.Kind, b.Expires = "manual", nil
	b.Offsite = &offsiteCopy{Status: "copied", At: time.Now().UTC()}
	if err := writeJSON(filepath.Join(stage, "meta.json"), b); err != nil {
		return b, err
	}
	return b, os.Rename(stage, filepath.Join(m.backupDir(), id))
}

// Delete a copy that is not in this server's library, such as one left by
// a server that no longer exists.
func (m *manager) deleteRemote(ctx context.Context, id string) error {
	if !idPattern.MatchString(id) {
		return fail(404, "Backup not found.")
	}
	if _, err := os.Stat(filepath.Join(m.backupDir(), id)); err == nil {
		return fail(409, "This backup is on this server. Delete it from the library instead.")
	}
	cfg, err := m.loadOffsite()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return fail(409, "Connect backup storage first.")
	}
	c, err := newS3(cfg)
	if err != nil {
		return err
	}
	for _, key := range []string{cfg.Prefix + id + "/meta.json", cfg.Prefix + id + "/data"} {
		if err := c.delete(ctx, key); err != nil {
			return fail(502, "Could not delete from storage: "+err.Error())
		}
	}
	m.logEvent(ctx, event{Name: "Backup storage", Kind: "storage", Action: "Delete storage copy", Result: "completed", Detail: id})
	return nil
}

// ---- a small S3 client

type s3 struct {
	cfg  offsiteConfig
	base *url.URL
	http *http.Client
	now  func() time.Time
}

func newS3(cfg offsiteConfig) (*s3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid storage endpoint %q", cfg.Endpoint)
	}
	return &s3{cfg: cfg, base: u, http: &http.Client{Timeout: 0}, now: time.Now}, nil
}

// RFC 3986 encoding as SigV4 expects it: unreserved characters stay.
func uriEncode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || (c == '/' && !slash) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := slices.Clone(q[k])
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func (c *s3) target(key string, q url.Values) *url.URL {
	u := *c.base
	path := "/" + key
	if c.cfg.PathStyle {
		path = "/" + c.cfg.Bucket
		if key != "" {
			path += "/" + key
		}
	} else {
		u.Host = c.cfg.Bucket + "." + c.base.Host
	}
	u.Path, u.RawPath = path, uriEncode(path, false)
	u.RawQuery = canonicalQuery(q)
	return &u
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// AWS Signature Version 4 over the request line, the host, every header set
// on the request and the payload hash.
func (c *s3) sign(req *http.Request, payloadHash string, t time.Time) {
	amzDate := t.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{req.Method, req.URL.EscapedPath(), canonicalQuery(req.URL.Query()), canon.String(), signed, payloadHash}, "\n")
	scope := day + "/" + c.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(request))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), day), c.cfg.Region), "s3"), "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey, scope, signed, hex.EncodeToString(hmacSHA256(key, toSign))))
}

type s3Failure struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var e s3Failure
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		switch e.Code {
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return fmt.Errorf("access denied (%s): check the keys and the bucket’s permissions", e.Code)
		case "NoSuchBucket":
			return errors.New("the bucket does not exist")
		}
		return fmt.Errorf("%s: %s", e.Code, e.Message)
	}
	return fmt.Errorf("storage answered HTTP %d", resp.StatusCode)
}

func (c *s3) do(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.target(key, q).String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	c.sign(req, payloadHash, c.now())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach storage: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp, nil
}

func discard(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

func (c *s3) putBytes(ctx context.Context, key string, data []byte) error {
	sum := sha256.Sum256(data)
	resp, err := c.do(ctx, http.MethodPut, key, nil, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(sum[:]))
	if err == nil {
		discard(resp)
	}
	return err
}

// Upload a file in one request, or in parts when it is large. Each request
// carries its payload hash, which every S3-compatible service accepts.
func (c *s3) putFile(ctx context.Context, key string, f *os.File, size int64) error {
	if size <= multipartAbove {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		resp, err := c.do(ctx, http.MethodPut, key, nil, f, size, hex.EncodeToString(h.Sum(nil)))
		if err == nil {
			discard(resp)
		}
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, emptyHash)
	if err != nil {
		return err
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&started)
	resp.Body.Close()
	if err != nil || started.UploadID == "" {
		return errors.New("storage did not start a multipart upload")
	}
	upload := url.Values{"uploadId": {started.UploadID}}
	abort := func() {
		if r, err := c.do(context.WithoutCancel(ctx), http.MethodDelete, key, upload, nil, 0, emptyHash); err == nil {
			discard(r)
		}
	}
	part := max(minPartSize, (size+9999)/10000)
	buf := make([]byte, part)
	type done struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []done
	for n := 1; ; n++ {
		read, err := io.ReadFull(f, buf)
		if read == 0 {
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			abort()
			return err
		}
		chunk := buf[:read]
		sum := sha256.Sum256(chunk)
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {started.UploadID}}
		resp, err := c.do(ctx, http.MethodPut, key, q, bytes.NewReader(chunk), int64(read), hex.EncodeToString(sum[:]))
		if err != nil {
			abort()
			return err
		}
		parts = append(parts, done{n, resp.Header.Get("ETag")})
		discard(resp)
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []done   `xml:"Part"`
	}{Parts: parts})
	sum := sha256.Sum256(body)
	resp, err = c.do(ctx, http.MethodPost, key, upload, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		abort()
		return err
	}
	defer resp.Body.Close()
	// Completion can fail with an error document and a 200 status.
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(answer, []byte("<Error>")) {
		abort()
		var e s3Failure
		xml.Unmarshal(answer, &e)
		return fmt.Errorf("completing the upload: %s %s", e.Code, e.Message)
	}
	return nil
}

func (c *s3) get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, 0, emptyHash)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (c *s3) getBytes(ctx context.Context, key string, limit int64) ([]byte, error) {
	body, _, err := c.get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, limit))
}

func (c *s3) delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil, 0, emptyHash)
	if err == nil {
		discard(resp)
	}
	return err
}

type s3Object struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
}

func (c *s3) list(ctx context.Context, prefix string) ([]s3Object, error) {
	var all []s3Object
	token := ""
	for range 100 {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, 0, emptyHash)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents  []s3Object `xml:"Contents"`
			Truncated bool       `xml:"IsTruncated"`
			Next      string     `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading the bucket listing: %w", err)
		}
		all = append(all, page.Contents...)
		if !page.Truncated || page.Next == "" {
			return all, nil
		}
		token = page.Next
	}
	return all, nil
}
