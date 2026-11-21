package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var assets embed.FS

func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func sendError(w http.ResponseWriter, err error) {
	var p *problem
	if !errors.As(err, &p) {
		log.Printf("control plane: %v", err)
		p = &problem{500, "Operation failed. Check the control-plane logs for details."}
	}
	sendJSON(w, p.status, map[string]string{"error": p.text})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fail(400, "Invalid request: "+err.Error())
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fail(400, "Send one JSON object.")
	}
	return nil
}

type endpoint func(http.ResponseWriter, *http.Request) error

func (f endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f(w, r); err != nil {
		sendError(w, err)
	}
}

func handler(m *manager, a auth) http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	api.HandleFunc("DELETE /api/session", func(w http.ResponseWriter, r *http.Request) {
		a.cookie(w, r, false)
		w.WriteHeader(http.StatusNoContent)
	})
	api.Handle("GET /api/host", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		host, err := m.host(r.Context())
		if err == nil {
			sendJSON(w, 200, host)
		}
		return err
	}))
	api.Handle("GET /api/instances", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.instances(r.Context())
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	}))
	api.Handle("POST /api/instances", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		var spec instanceSpec
		if err := decode(w, r, &spec); err != nil {
			return err
		}
		if err := m.provision(r.Context(), spec, nil); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": spec.Name})
		return nil
	}))
	for _, op := range []string{"start", "stop", "delete"} {
		pattern := "POST /api/instances/{name}/" + op
		if op == "delete" {
			pattern = "DELETE /api/instances/{name}"
		}
		api.Handle(pattern, endpoint(func(w http.ResponseWriter, r *http.Request) error {
			if err := m.change(r.Context(), r.PathValue("name"), op); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}))
	}
	api.Handle("GET /api/instances/{name}/connection", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		db, err := m.record(r.PathValue("name"))
		if err != nil {
			return err
		}
		u := url.URL{Scheme: engines[db.Engine].scheme, User: url.UserPassword("openrds", db.Password),
			Host: "127.0.0.1:" + strconv.Itoa(db.Port), Path: "/app"}
		internal := u
		alias := "openrds-" + db.Name
		internal.Host = alias + ":" + strconv.Itoa(engines[db.Engine].port)
		sendJSON(w, 200, map[string]any{"host": "127.0.0.1", "port": db.Port, "user": "openrds", "database": "app", "password": db.Password, "uri": u.String(),
			"network": db.Project + "_default", "networkHost": alias, "networkURI": internal.String()})
		return nil
	}))
	api.Handle("GET /api/instances/{name}/metrics", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		u, err := m.metrics(r.Context(), r.PathValue("name"))
		if err != nil {
			return err
		}
		sendJSON(w, 200, u)
		return nil
	}))
	api.Handle("GET /api/instances/{name}/logs", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		text, err := m.logs(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 200, map[string]string{"text": text})
		}
		return err
	}))
	api.Handle("GET /api/activity", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.readActivity()
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	}))
	api.Handle("GET /api/backups", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.backups()
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	}))
	api.Handle("POST /api/instances/{name}/backups", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.makeBackup(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 201, b)
		}
		return err
	}))
	api.Handle("GET /api/backups/{id}/download", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.backup(r.PathValue("id"))
		if err != nil {
			return err
		}
		f, err := m.openDump(b.ID)
		if err != nil {
			return err
		}
		defer f.Close()
		name := b.Name + "-" + b.Created.Format("20060102-150405") + engines[b.Engine].suffix
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		http.ServeContent(w, r, name, b.Created, f)
		return nil
	}))
	api.Handle("POST /api/backups/{id}/restore", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.backup(r.PathValue("id"))
		if err != nil {
			return err
		}
		var spec instanceSpec
		if err := decode(w, r, &spec); err != nil {
			return err
		}
		if err := m.provision(r.Context(), spec, &b); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": spec.Name})
		return nil
	}))
	api.Handle("DELETE /api/backups/{id}", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.backup(r.PathValue("id"))
		if err != nil {
			return err
		}
		if err := m.deleteBackup(b); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		if !a.bearer(r) {
			sendError(w, fail(401, "The admin token was not accepted."))
			return
		}
		a.cookie(w, r, true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			sendError(w, fail(401, "Sign in with your admin token."))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		api.ServeHTTP(w, r.WithContext(ctx))
	})
	web, _ := fs.Sub(assets, "web")
	static := http.NewServeMux()
	static.Handle("GET /", http.FileServerFS(web))
	mux.Handle("/", static)
	safe := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		safe.ServeHTTP(w, r)
	})
}
