package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The examples from the Amazon S3 documentation for Signature Version 4.
func TestSigV4MatchesAWSExamples(t *testing.T) {
	c, err := newS3(offsiteConfig{Endpoint: "https://s3.amazonaws.com", Region: "us-east-1", Bucket: "examplebucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	get, _ := http.NewRequest("GET", c.target("test.txt", nil).String(), nil)
	get.Header.Set("Range", "bytes=0-9")
	c.sign(get, emptyHash, when)
	if a := get.Header.Get("Authorization"); !strings.HasSuffix(a, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41") {
		t.Fatalf("GET object: %s", a)
	}
	list, _ := http.NewRequest("GET", c.target("", map[string][]string{"max-keys": {"2"}, "prefix": {"J"}}).String(), nil)
	c.sign(list, emptyHash, when)
	if a := list.Header.Get("Authorization"); !strings.HasSuffix(a, "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7") {
		t.Fatalf("list objects: %s", a)
	}
}

// A minimal S3 service: path-style buckets, multipart uploads and V2
// listings. It refuses unsigned requests and bodies that do not match
// their declared hash.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	fail    bool
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-key/") || r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		w.WriteHeader(403)
		fmt.Fprint(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>bad</Message></Error>")
		return
	}
	if f.fail {
		w.WriteHeader(500)
		fmt.Fprint(w, "<Error><Code>InternalError</Code><Message>down</Message></Error>")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "backups" {
		w.WriteHeader(404)
		fmt.Fprint(w, "<Error><Code>NoSuchBucket</Code><Message>no</Message></Error>")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && key == "":
		var b strings.Builder
		b.WriteString("<ListBucketResult><IsTruncated>false</IsTruncated>")
		for k, v := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(v))
			}
		}
		b.WriteString("</ListBucketResult>")
		fmt.Fprint(w, b.String())
	case r.Method == "POST" && q.Has("uploads"):
		id := randomHex(8)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == "PUT" && q.Has("uploadId"):
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
	case r.Method == "POST" && q.Has("uploadId"):
		var done struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		xml.Unmarshal(body, &done)
		var all []byte
		for _, p := range done.Parts {
			all = append(all, f.uploads[q.Get("uploadId")][p.Number]...)
		}
		f.objects[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	case r.Method == "PUT":
		f.objects[key] = body
	case r.Method == "GET":
		v, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>no</Message></Error>")
			return
		}
		w.Write(v)
	case r.Method == "DELETE":
		delete(f.objects, key)
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(204)
	}
}

func storageConfig(url string) offsiteConfig {
	return offsiteConfig{Endpoint: url, Bucket: "backups", Prefix: "/team//openrds/", AccessKey: "test-key", SecretKey: "test-secret", PathStyle: true}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBackupsAreCopiedImportedAndDeletedInStorage(t *testing.T) {
	store, srv := newFakeS3(t)
	f := newFakeDocker()
	m := testManager(t, f.run)
	ctx := t.Context()
	if err := m.provision(ctx, instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}, nil); err != nil {
		t.Fatal(err)
	}
	b, err := m.makeBackup(ctx, "orders", "manual")
	if err != nil {
		t.Fatal(err)
	}
	bad := storageConfig(srv.URL)
	bad.Bucket = "missing"
	if err := m.saveOffsite(ctx, bad); status(err) != 502 || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a wrong bucket must be refused before saving: %v", err)
	}
	if err := m.saveOffsite(ctx, storageConfig(srv.URL)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := m.loadOffsite()
	if cfg.Prefix != "team/openrds/" {
		t.Fatalf("prefix: %q", cfg.Prefix)
	}
	copied := func() bool {
		m.syncOffsite()
		got, _ := m.backup(b.ID)
		return got.Offsite != nil && got.Offsite.Status == "copied"
	}
	eventually(t, "the copy", copied)
	data, _ := os.ReadFile(filepath.Join(m.backupDir(), b.ID, "data"))
	if !bytes.Equal(store.objects[cfg.Prefix+b.ID+"/data"], data) || len(store.objects[cfg.Prefix+b.ID+"/meta.json"]) == 0 {
		t.Fatalf("storage holds %v", store.keys(""))
	}
	if v, _ := m.offsiteStatus(false); v.Copied != 1 || v.Pending != 0 || v.AccessKey != "" || v.Bucket != "backups" {
		t.Fatalf("status: %+v", v)
	}
	if v, _ := m.offsiteStatus(true); v.AccessKey != "test-key" {
		t.Fatal("admins see the access key ID")
	}
	if err := m.deleteRemote(ctx, b.ID); status(err) != 409 {
		t.Fatal("a copy of a backup in the library must be deleted through the library")
	}

	// A lost server: the library is gone, the copy is not.
	os.RemoveAll(filepath.Join(m.backupDir(), b.ID))
	remote, err := m.remoteBackups(ctx)
	if err != nil || len(remote) != 1 || remote[0].ID != b.ID || remote[0].Local {
		t.Fatalf("remote: %+v %v", remote, err)
	}
	imported, err := m.importBackup(ctx, b.ID)
	if err != nil || imported.Kind != "manual" || imported.Expires != nil {
		t.Fatalf("import: %+v %v", imported, err)
	}
	if again, _ := os.ReadFile(filepath.Join(m.backupDir(), b.ID, "data")); !bytes.Equal(again, data) {
		t.Fatal("the imported data differs")
	}
	if _, err := m.importBackup(ctx, b.ID); status(err) != 409 {
		t.Fatal("importing twice must be refused")
	}

	// Deleting from the library deletes the copy.
	if err := m.deleteBackup(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the copy to be deleted", func() bool { return len(store.keys(cfg.Prefix+b.ID)) == 0 })

	// A failed copy is recorded and counted, then retried later.
	store.mu.Lock()
	store.fail = true // before the backup, which starts its own copy at once
	store.mu.Unlock()
	b2, _ := m.makeBackup(ctx, "orders", "manual")
	eventually(t, "the failure", func() bool {
		m.syncOffsite()
		got, _ := m.backup(b2.ID)
		return got.Offsite != nil && got.Offsite.Status == "failed"
	})
	if v, _ := m.offsiteStatus(false); v.Failed != 1 || v.LastError == "" {
		t.Fatalf("status after a failure: %+v", v)
	}
	if err := m.disableOffsite(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.offsiteStatus(true); v.Enabled {
		t.Fatal("disconnecting must stop copying")
	}
}

func TestLargeBackupsUploadInParts(t *testing.T) {
	store, srv := newFakeS3(t)
	defer func(a, p int64) { multipartAbove, minPartSize = a, p }(multipartAbove, minPartSize)
	multipartAbove, minPartSize = 1024, 1000
	c, _ := newS3(offsiteConfig{Endpoint: srv.URL, Region: "us-east-1", Bucket: "backups", AccessKey: "test-key", SecretKey: "s", PathStyle: true})
	want := bytes.Repeat([]byte("0123456789abcdef"), 200) // 3200 bytes, four parts
	path := filepath.Join(t.TempDir(), "data")
	os.WriteFile(path, want, 0600)
	file, _ := os.Open(path)
	defer file.Close()
	if err := c.putFile(context.Background(), "big/data", file, int64(len(want))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(store.objects["big/data"], want) || len(store.uploads) != 1 {
		t.Fatalf("multipart result: %d bytes", len(store.objects["big/data"]))
	}
}

func TestStorageSettingsAreChecked(t *testing.T) {
	m := testManager(t, nil)
	good := offsiteConfig{Endpoint: "https://s3.eu-west-1.amazonaws.com/", Bucket: "my-backups", AccessKey: "AKIA1", SecretKey: "secret", Prefix: "/prod/db/"}
	cfg, err := m.checkOffsite(good)
	if err != nil || cfg.Endpoint != "https://s3.eu-west-1.amazonaws.com" || cfg.Region != "us-east-1" || cfg.Prefix != "prod/db/" {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, change := range []func(*offsiteConfig){
		func(c *offsiteConfig) { c.Endpoint = "ftp://host" },
		func(c *offsiteConfig) { c.Endpoint = "https://host/path" },
		func(c *offsiteConfig) { c.Bucket = "UPPER" },
		func(c *offsiteConfig) { c.Prefix = "a/../b" },
		func(c *offsiteConfig) { c.Region = "Bad Region" },
		func(c *offsiteConfig) { c.AccessKey = "" },
	} {
		c := good
		change(&c)
		if _, err := m.checkOffsite(c); status(err) != 400 {
			t.Errorf("%+v accepted", c)
		}
	}
	writeJSON(m.offsitePath(), offsiteConfig{Enabled: true, SecretKey: "kept-secret"})
	keep := good
	keep.SecretKey = ""
	if cfg, err := m.checkOffsite(keep); err != nil || cfg.SecretKey != "kept-secret" {
		t.Fatal("an empty secret must keep the saved one")
	}
}
