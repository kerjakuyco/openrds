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
	"maps"
	"net/http"
	"net/url"
	"strconv"
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

var chartRanges = map[string]time.Duration{"1h": time.Hour, "3h": 3 * time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour}

type engineView struct {
	Key          string      `json:"key"`
	Title        string      `json:"title"`
	DefaultPort  int         `json:"defaultPort"`
	MinMemoryMiB int         `json:"minMemoryMiB"`
	User         string      `json:"user"`
	Database     string      `json:"database"`
	SizeUnits    []string    `json:"sizeUnits,omitempty"`
	Parameters   []parameter `json:"parameters"`
}

func engineCatalog() []engineView {
	list := make([]engineView, 0, len(engineOrder))
	for _, key := range engineOrder {
		e := engines[key]
		list = append(list, engineView{key, e.title, e.hostPort, e.minMemory, e.user, e.database, sizeUnits[key], e.parameters})
	}
	return list
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
	api.Handle("GET /api/engines", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		sendJSON(w, 200, engineCatalog())
		return nil
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
	for _, op := range []string{"start", "stop", "reboot"} {
		api.Handle("POST /api/instances/{name}/"+op, endpoint(func(w http.ResponseWriter, r *http.Request) error {
			if err := m.change(r.Context(), r.PathValue("name"), op); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}))
	}
	api.Handle("DELETE /api/instances/{name}", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		final := r.URL.Query().Get("finalSnapshot")
		if final != "" && final != "true" && final != "false" {
			return fail(400, "Set finalSnapshot to true or false.")
		}
		if err := m.remove(r.Context(), r.PathValue("name"), final == "true"); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	api.Handle("POST /api/instances/{name}/modify", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		var mod modification
		if err := decode(w, r, &mod); err != nil {
			return err
		}
		result, err := m.modify(r.Context(), r.PathValue("name"), mod)
		if err == nil {
			sendJSON(w, 200, result)
		}
		return err
	}))
	api.Handle("GET /api/instances/{name}/history", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		db, err := m.record(r.PathValue("name"))
		if err != nil {
			return err
		}
		key := r.URL.Query().Get("range")
		if key == "" {
			key = "1h"
		}
		span, ok := chartRanges[key]
		if !ok {
			return fail(400, "Choose a range of 1h, 3h, 6h, 12h or 24h.")
		}
		sendJSON(w, 200, m.history.view(db.Project, span, time.Now()))
		return nil
	}))
	api.Handle("GET /api/instances/{name}/connection", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		db, err := m.record(r.PathValue("name"))
		if err != nil {
			return err
		}
		eng := engines[db.Engine]
		u := url.URL{Scheme: eng.scheme, User: url.UserPassword(eng.user, db.Password),
			Host: "127.0.0.1:" + strconv.Itoa(db.Port), Path: "/" + eng.database}
		internal := u
		alias := "openrds-" + db.Name
		internal.Host = alias + ":" + strconv.Itoa(engines[db.Engine].port)
		sendJSON(w, 200, map[string]any{"host": "127.0.0.1", "port": db.Port, "user": eng.user, "database": eng.database, "password": db.Password, "uri": u.String(),
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
	api.Handle("POST /api/instances/{name}/disk", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			DiskGiB int `json:"diskGiB"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		if err := m.resizeDisk(r.Context(), r.PathValue("name"), body.DiskGiB); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	api.Handle("GET /api/instances/{name}/storage", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		used, err := m.volumeUsage(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 200, map[string]int64{"usedBytes": used})
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
		b, err := m.makeBackup(r.Context(), r.PathValue("name"), "manual")
		if err == nil {
			sendJSON(w, 201, b)
		}
		return err
	}))
	api.Handle("POST /api/backups/{id}/keep", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.keepBackup(r.PathValue("id"))
		if err == nil {
			sendJSON(w, 200, b)
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
		spec := instanceSpec{DiskGiB: b.DiskGiB}
		if err := decode(w, r, &spec); err != nil {
			return err
		}
		if spec.Parameters == nil {
			spec.Parameters = maps.Clone(b.Parameters) // omitted or null inherits; {} restores defaults
		}
		if err := m.provision(r.Context(), spec, &b); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": spec.Name})
		return nil
	}))
	api.Handle("DELETE /api/backups/{id}", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		if err := m.deleteBackup(r.PathValue("id")); err != nil {
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
		w.Header().Set("Cache-Control", "no-store")
		safe.ServeHTTP(w, r)
	})
}
