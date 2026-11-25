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
	Key          string          `json:"key"`
	Title        string          `json:"title"`
	DefaultPort  int             `json:"defaultPort"`
	MinMemoryMiB int             `json:"minMemoryMiB"`
	User         string          `json:"user"`
	Database     string          `json:"database"`
	SizeUnits    []string        `json:"sizeUnits,omitempty"`
	Parameters   []parameter     `json:"parameters"`
	Versions     []engineVersion `json:"versions"`
	Default      string          `json:"defaultVersion"`
}

func engineCatalog() []engineView {
	list := make([]engineView, 0, len(engineOrder))
	for _, key := range engineOrder {
		e := engines[key]
		list = append(list, engineView{key, e.title, e.hostPort, e.minMemory, e.user, e.database, sizeUnits[key], e.parameters, engineVersions[key].list, defaultVersion(key)})
	}
	return list
}

// Reads end when the client goes away. Changes keep going: a snapshot or a
// restore of a large database can take hours, and a closed browser tab or a
// dropped SSH tunnel must not leave it half done. The Events page shows how
// a change ended.
const (
	readLimit   = 30 * time.Minute
	changeLimit = 24 * time.Hour
)

func requestContext(ctx context.Context, method string) (context.Context, context.CancelFunc) {
	if method == http.MethodGet || method == http.MethodHead {
		return context.WithTimeout(ctx, readLimit)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), changeLimit)
}

type endpoint func(http.ResponseWriter, *http.Request) error

func (f endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f(w, r); err != nil {
		sendError(w, err)
	}
}

func handler(m *manager, a auth) http.Handler {
	api := http.NewServeMux()
	add := func(pattern string, level int, h endpoint) { api.Handle(pattern, require(level, h)) }
	add("GET /api/host", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		host, err := m.host(r.Context())
		if err == nil {
			sendJSON(w, 200, host)
		}
		return err
	})
	add("GET /api/instances", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.instances(r.Context())
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	})
	add("GET /api/engines", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		sendJSON(w, 200, engineCatalog())
		return nil
	})
	add("POST /api/instances", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var spec instanceSpec
		if err := decode(w, r, &spec); err != nil {
			return err
		}
		if err := m.prepareNetwork(&spec); err != nil {
			return err
		}
		if err := m.provision(r.Context(), spec, nil); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": spec.Name})
		return nil
	})
	for _, op := range []string{"start", "stop", "reboot"} {
		add("POST /api/instances/{name}/"+op, roleOperator, func(w http.ResponseWriter, r *http.Request) error {
			if err := m.change(r.Context(), r.PathValue("name"), op); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		})
	}
	add("DELETE /api/instances/{name}", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		final := r.URL.Query().Get("finalSnapshot")
		if final != "" && final != "true" && final != "false" {
			return fail(400, "Set finalSnapshot to true or false.")
		}
		if err := m.remove(r.Context(), r.PathValue("name"), final == "true"); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("POST /api/instances/{name}/modify", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var mod modification
		if err := decode(w, r, &mod); err != nil {
			return err
		}
		result, err := m.modify(r.Context(), r.PathValue("name"), mod)
		if err == nil {
			sendJSON(w, 200, result)
		}
		return err
	})
	add("GET /api/instances/{name}/history", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
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
	})
	add("GET /api/instances/{name}/connection", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		db, login, password, err := m.credentials(r.Context(), r.PathValue("name"), r.URL.Query().Get("user"))
		if err != nil {
			return err
		}
		eng := engines[db.Engine]
		scheme := eng.scheme
		if db.Engine == "valkey" && db.RequireTLS {
			scheme = "rediss"
		}
		u := url.URL{Scheme: scheme, User: url.UserPassword(login, password),
			Host: "127.0.0.1:" + strconv.Itoa(db.Port), Path: "/" + eng.database}
		internal := u
		alias := "openrds-" + db.Name
		internal.Host = alias + ":" + strconv.Itoa(engines[db.Engine].port)
		sendJSON(w, 200, map[string]any{"host": "127.0.0.1", "port": db.Port, "user": login, "database": eng.database, "password": password, "uri": u.String(),
			"network": db.Project + "_default", "networkHost": alias, "networkURI": internal.String()})
		return nil
	})
	add("POST /api/instances/{name}/password", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		password, err := m.changePassword(r.Context(), r.PathValue("name"), "", body.Password)
		if err == nil {
			sendJSON(w, 200, map[string]string{"password": password})
		}
		return err
	})
	add("POST /api/instances/{name}/users", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Name     string `json:"name"`
			Access   string `json:"access"`
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		password, err := m.createUser(r.Context(), r.PathValue("name"), body.Name, body.Access, body.Password)
		if err == nil {
			sendJSON(w, 201, map[string]string{"name": body.Name, "password": password})
		}
		return err
	})
	add("POST /api/instances/{name}/users/{user}/password", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		password, err := m.changePassword(r.Context(), r.PathValue("name"), r.PathValue("user"), body.Password)
		if err == nil {
			sendJSON(w, 200, map[string]string{"password": password})
		}
		return err
	})
	add("DELETE /api/instances/{name}/users/{user}", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.deleteUser(r.Context(), r.PathValue("name"), r.PathValue("user")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("GET /api/instances/{name}/connections", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.connectionAttempts(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	})
	add("GET /api/tls/ca.pem", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		ca, err := m.authority()
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="openrds-ca.pem"`)
		_, err = w.Write(ca.pem)
		return err
	})
	add("GET /api/instances/{name}/metrics", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		u, err := m.metrics(r.Context(), r.PathValue("name"))
		if err != nil {
			return err
		}
		sendJSON(w, 200, u)
		return nil
	})
	add("GET /api/instances/{name}/logs", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		text, err := m.logs(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 200, map[string]string{"text": text})
		}
		return err
	})
	add("POST /api/instances/{name}/disk", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
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
	})
	add("GET /api/instances/{name}/storage", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		used, err := m.volumeUsage(r.Context(), r.PathValue("name"))
		if err == nil {
			sendJSON(w, 200, map[string]int64{"usedBytes": used})
		}
		return err
	})
	add("GET /api/activity", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.readActivity()
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	})
	add("GET /api/backups", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		items, err := m.backups()
		if err == nil {
			sendJSON(w, 200, items)
		}
		return err
	})
	add("POST /api/instances/{name}/backups", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.makeBackup(r.Context(), r.PathValue("name"), "manual")
		if err == nil {
			sendJSON(w, 201, b)
		}
		return err
	})
	add("POST /api/backups/{id}/keep", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.keepBackup(r.Context(), r.PathValue("id"))
		if err == nil {
			sendJSON(w, 200, b)
		}
		return err
	})
	add("GET /api/backups/{id}/download", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.backup(r.PathValue("id"))
		if err != nil {
			return err
		}
		f, err := m.openDump(b.ID)
		if err != nil {
			return err
		}
		defer f.Close()
		name := b.fileName()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		http.ServeContent(w, r, name, b.Created, f)
		return nil
	})
	add("POST /api/backups/{id}/restore", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
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
		if err := m.prepareNetwork(&spec); err != nil {
			return err
		}
		if err := m.provision(r.Context(), spec, &b); err != nil {
			return err
		}
		sendJSON(w, 201, map[string]string{"name": spec.Name})
		return nil
	})
	add("DELETE /api/backups/{id}", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.deleteBackup(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	add("GET /api/storage", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		id, _ := identityFrom(r.Context())
		v, err := m.offsiteStatus(id.Role == "admin")
		if err == nil {
			sendJSON(w, 200, v)
		}
		return err
	})
	add("PUT /api/storage", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var cfg offsiteConfig
		if err := decode(w, r, &cfg); err != nil {
			return err
		}
		if err := m.saveOffsite(r.Context(), cfg); err != nil {
			return err
		}
		v, err := m.offsiteStatus(true)
		if err == nil {
			sendJSON(w, 200, v)
		}
		return err
	})
	add("POST /api/storage/test", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var in offsiteConfig
		if err := decode(w, r, &in); err != nil {
			return err
		}
		cfg, err := m.checkOffsite(in)
		if err == nil {
			err = testOffsite(r.Context(), cfg)
		}
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
		return err
	})
	add("DELETE /api/storage", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.disableOffsite(r.Context()); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	notifyRoutes(add, m)
	insightRoutes(add, m)
	add("GET /api/storage/backups", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		list, err := m.remoteBackups(r.Context())
		if err == nil {
			sendJSON(w, 200, list)
		}
		return err
	})
	add("POST /api/storage/backups/{id}/import", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		b, err := m.importBackup(r.Context(), r.PathValue("id"))
		if err == nil {
			sendJSON(w, 201, b)
		}
		return err
	})
	add("DELETE /api/storage/backups/{id}", roleOperator, func(w http.ResponseWriter, r *http.Request) error {
		if err := m.deleteRemote(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	accountRoutes(mux, add, m, a)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.identify(r)
		if !ok {
			sendError(w, fail(401, "Sign in to continue."))
			return
		}
		ctx, cancel := requestContext(withIdentity(r.Context(), id), r.Method)
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
