package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runFunc func(context.Context, []string, io.Reader, io.Writer, ...string) error

func (f runFunc) run(c context.Context, e []string, in io.Reader, out io.Writer, args ...string) error {
	return f(c, e, in, out, args...)
}

func testManager(t *testing.T, run runFunc) *manager {
	t.Helper()
	m, err := openManager(t.TempDir(), "engines", run)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSessionAndSecretBoundaries(t *testing.T) {
	var project string
	m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
		if args[0] == "compose" {
			project = args[2]
		} else if args[0] == "ps" {
			fmt.Fprintf(out, "%s|running|Up 5 minutes (healthy)\n", project)
		}
		return nil
	})
	token, err := adminToken(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := adminToken(m.dir)
	if err != nil || again != token {
		t.Fatalf("token did not survive restart: %v", err)
	}
	h := handler(m, testAuth(t, m, token))
	request := func(method, path, body string, cookie *http.Cookie, bearer, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	spec := `{"name":"orders","engine":"mysql","port":13306}`
	if w := request("POST", "/api/instances", spec, nil, "", ""); w.Code != 401 || project != "" {
		t.Fatal("provisioning must require authentication")
	}
	login := request("POST", "/api/session", "", nil, token, "")
	if login.Code != 204 || len(login.Result().Cookies()) != 1 {
		t.Fatal("expected a browser session")
	}
	cookie := login.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || strings.Contains(cookie.Value, token) {
		t.Fatal("session must not expose the admin token")
	}
	if w := request("POST", "/api/instances", spec, cookie, "", "https://another.example"); w.Code != 403 {
		t.Fatalf("cross-origin mutation returned %d", w.Code)
	}
	if w := request("POST", "/api/instances", spec, cookie, "", ""); w.Code != 201 {
		t.Fatalf("provisioning returned %d: %s", w.Code, w.Body)
	}
	r, err := m.record("orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/instances", "/api/activity", "/api/backups"} {
		w := request("GET", path, "", cookie, "", "")
		if w.Code != 200 || strings.Contains(w.Body.String(), r.Password) || strings.Contains(w.Body.String(), r.RootPassword) {
			t.Fatalf("unexpected or sensitive response from %s", path)
		}
	}
	w := request("GET", "/api/instances/orders/connection", "", nil, token, "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), r.Password) || strings.Contains(w.Body.String(), r.RootPassword) {
		t.Fatal("connection endpoint did not return only the requested user's credentials")
	}
	exp := fmt.Sprint(time.Now().Add(-time.Second).Unix())
	cookie.Value = exp + "." + (auth{token: token}).sign(exp)
	if w := request("GET", "/api/activity", "", cookie, "", ""); w.Code != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestBackupSurvivesSourceDeletionAndRestoresToFreshProject(t *testing.T) {
	for _, key := range []string{"postgres", "mysql", "valkey"} {
		t.Run(key, func(t *testing.T) {
			ctx := t.Context()
			dump := []byte("logical\x00backup\xff")
			var restored []byte
			var deleted string
			m := testManager(t, func(_ context.Context, _ []string, in io.Reader, out io.Writer, args ...string) error {
				if args[0] != "compose" || filepath.Base(args[4]) != key+".yaml" {
					return fmt.Errorf("wrong engine template")
				}
				switch verb := composeVerb(args); verb {
				case "exec", "run":
					if out != nil { // a dump; Valkey's reads the password first
						_, err := out.Write(dump)
						return err
					}
					if in != nil {
						var err error
						restored, err = io.ReadAll(in)
						return err
					}
				case "down", "stop", "up":
					if verb == "down" {
						deleted = args[2]
					}
				}
				return nil
			})
			spec := instanceSpec{Name: "orders", Engine: key, Port: 15432}
			if err := m.provision(ctx, spec, nil); err != nil {
				t.Fatal(err)
			}
			source, _ := m.record("orders")
			b, err := m.makeBackup(ctx, "orders", "manual")
			if err != nil || b.Bytes != int64(len(dump)) {
				t.Fatalf("backup: %+v %v", b, err)
			}
			if err := m.remove(ctx, "orders", false); err != nil || deleted != source.Project {
				t.Fatalf("wrong project deleted: %s, %v", deleted, err)
			}
			reopened, err := openManager(m.dir, m.templates, m.docker)
			if err != nil {
				t.Fatal(err)
			}
			all, err := reopened.backups()
			if err != nil || len(all) != 1 || all[0].ID != b.ID {
				t.Fatal("backup did not survive deletion and control-plane restart")
			}
			if err := reopened.provision(ctx, instanceSpec{Name: "restored", Port: 15432}, &all[0]); err != nil {
				t.Fatal(err)
			}
			target, err := reopened.record("restored")
			if err != nil || target.Engine != key || target.Project == source.Project || target.Password == source.Password || target.PendingRestore || !bytes.Equal(restored, dump) {
				t.Fatal("restore did not use the original bytes and a fresh target")
			}
		})
	}
}

func TestFailureKeepsRecoverableState(t *testing.T) {
	var failure string
	m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
		if args[0] == "compose" && composeVerb(args) == "exec" && out != nil {
			fmt.Fprint(out, "dump data")
		}
		if args[0] == "compose" && composeVerb(args) == failure {
			return errors.New("engine unavailable")
		}
		return nil
	})
	spec := instanceSpec{Name: "orders", Engine: "postgres", Port: 15432}
	ctx := t.Context()
	failure = "up"
	if err := m.provision(ctx, spec, nil); err == nil {
		t.Fatal("failed provision reported success")
	}
	failure = ""
	if err := m.change(ctx, spec.Name, "start"); err != nil {
		t.Fatalf("could not retry: %v", err)
	}
	failure = "exec"
	if _, err := m.makeBackup(ctx, spec.Name, "manual"); err == nil {
		t.Fatal("failed backup reported success")
	}
	if list, err := m.backups(); err != nil || len(list) != 0 {
		t.Fatal("partial backup published")
	}
	failure = ""
	b, err := m.makeBackup(ctx, spec.Name, "manual")
	if err != nil {
		t.Fatal(err)
	}
	failure = "exec"
	if err := m.provision(ctx, instanceSpec{Name: "restore", Port: 15433}, &b); err == nil {
		t.Fatal("failed restore reported success")
	}
	r, err := m.record("restore")
	if err != nil || !r.PendingRestore {
		t.Fatal("failed restore must stay marked incomplete")
	}
	if _, err := m.makeBackup(ctx, "restore", "manual"); err == nil {
		t.Fatal("incomplete restore accepted as a recovery point")
	}
	failure = "down"
	if err := m.remove(ctx, spec.Name, false); err == nil {
		t.Fatal("failed delete reported success")
	}
	if _, err := m.record(spec.Name); err != nil {
		t.Fatal("failed delete lost the instance record")
	}
}

func TestConcurrentPortReservation(t *testing.T) {
	m := testManager(t, nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func() {
			<-start
			_, release, err := m.begin(name, &instanceSpec{Name: name, Engine: "postgres", Port: 15432}, false)
			if release != nil {
				release()
			}
			results <- err
		}()
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		var p *problem
		switch {
		case err == nil:
			wins++
		case errors.As(err, &p) && p.status == 409:
			conflicts++
		default:
			t.Fatalf("unexpected reservation failure: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("two instances reserved the same port")
	}
}
