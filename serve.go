package main

// Responses on the wire: the console's pages and scripts are compressed once
// at start and revalidated with ETags, so a reload transfers almost nothing,
// and large JSON replies are compressed for browsers that accept it, which
// matters over an SSH tunnel.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

type asset struct {
	data, gz   []byte // gz is nil when compressing doesn't help
	etag, kind string
}

type assetServer map[string]asset

// Read every file once. Fonts and images that are already compressed are
// served as they are.
func loadAssets(files fs.FS) (assetServer, error) {
	assets := assetServer{}
	err := fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		a := asset{data: data, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, kind: mime.TypeByExtension(path.Ext(name))}
		if a.kind == "" {
			a.kind = http.DetectContentType(data)
		}
		if compressible(a.kind) {
			var buf bytes.Buffer
			z, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			z.Write(data)
			z.Close()
			if buf.Len() < len(data)*9/10 {
				a.gz = buf.Bytes()
			}
		}
		assets["/"+name] = a
		return nil
	})
	return assets, err
}

func compressible(kind string) bool {
	return strings.HasPrefix(kind, "text/") || strings.HasPrefix(kind, "image/svg") || strings.Contains(kind, "json")
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(coding), "gzip") && strings.ReplaceAll(params, " ", "") != "q=0" {
			return true
		}
	}
	return false
}

// Browsers check with the ETag before using a cached copy, so a new
// version of the console shows at the next load.
func (a assetServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path
	switch name {
	case "/index.html": // like http.FileServer: the page lives at /
		http.Redirect(w, r, "./", http.StatusMovedPermanently)
		return
	case "/":
		name = "/index.html"
	}
	f, ok := a[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.kind)
	h.Set("Cache-Control", "no-cache")
	data, etag := f.data, f.etag
	if f.gz != nil {
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			data, etag = f.gz, strings.TrimSuffix(f.etag, `"`)+`-gz"`
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// ---------- JSON

// The client accepts gzip, so a large JSON reply goes compressed. Anything
// else written to it, such as a backup download, passes through untouched,
// still sent straight from the file.
type gzipAllowed struct{ http.ResponseWriter }

func (w gzipAllowed) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w gzipAllowed) ReadFrom(r io.Reader) (int64, error) { return io.Copy(w.ResponseWriter, r) }

// Replies smaller than about one network packet are not worth compressing.
const compressAbove = 1400

var (
	jsonBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	gzipWriters = sync.Pool{New: func() any { z, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return z }}
)

func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	buf := jsonBuffers.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= 1<<20 { // a rare large reply doesn't stay in memory
			jsonBuffers.Put(buf)
		}
	}()
	if err := json.NewEncoder(buf).Encode(data); err != nil {
		log.Printf("encoding a reply: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, ok := w.(gzipAllowed); !ok || buf.Len() < compressAbove {
		w.WriteHeader(status)
		w.Write(buf.Bytes())
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.WriteHeader(status)
	z := gzipWriters.Get().(*gzip.Writer)
	z.Reset(w)
	z.Write(buf.Bytes())
	z.Close()
	gzipWriters.Put(z)
}
