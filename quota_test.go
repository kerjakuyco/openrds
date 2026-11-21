package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestQuotaRefusalDoesNotReserveAnInstance(t *testing.T) {
	for _, tc := range []struct {
		diagnostic string
		status     int
	}{
		{"No such image: openrds-disk-helper:local", 422},
		{"cannot connect to the daemon", 503},
	} {
		t.Run(tc.diagnostic, func(t *testing.T) {
			probes := 0
			m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, _ io.Writer, args ...string) error {
				if args[0] == "image" && args[1] == "inspect" {
					probes++
					return errors.New(tc.diagnostic)
				}
				t.Fatalf("unexpected Docker operation: %v", args)
				return nil
			})
			for _, size := range []int{-1, maxDiskGiB + 1, 1, 20} {
				want := tc.status
				if size < 0 || size > maxDiskGiB {
					want = 400
				}
				err := m.provision(t.Context(), instanceSpec{Name: "test", Engine: "mysql", Port: 13306, DiskGiB: size}, nil)
				var p *problem
				if !errors.As(err, &p) || p.status != want {
					t.Fatalf("size %d: got %v, want HTTP %d", size, err, want)
				}
			}
			rows, err := m.records()
			if err != nil || len(rows) != 0 || probes != 1 {
				t.Fatalf("rows=%d probes=%d err=%v", len(rows), probes, err)
			}
		})
	}
}

func TestCappedInstanceDoesNotReuseOrDeleteMismatchedVolume(t *testing.T) {
	var name, inspect string
	var ups, downs, attaches int
	m := testManager(t, func(_ context.Context, env []string, _ io.Reader, out io.Writer, args ...string) error {
		switch args[0] {
		case "image":
			return nil
		case "run":
			if slices.Contains(args, "attach") {
				attaches++
			}
			return nil
		case "volume":
			switch args[1] {
			case "create":
				return nil
			case "inspect":
				if strings.Contains(strings.Join(args, " "), "Mountpoint") {
					fmt.Fprint(out, "/var/lib/docker/volumes/openrds_disk_images/_data")
					return nil
				}
				fmt.Fprint(out, inspect)
			case "ls":
				fmt.Fprint(out, name)
			}
			return nil
		}
		if !strings.Contains(strings.Join(args, " "), "volume-quota.yaml") || !slices.Contains(env, "OPENRDS_DISK_LINK="+name) && name != "" {
			if name == "" || !slices.Contains(env, "OPENRDS_DISK_LINK="+strings.TrimSuffix(name, "_data")) {
				t.Fatalf("capped operation lacks its quota overlay: %v", env)
			}
		}
		if strings.Contains(strings.Join(args, " "), " up ") {
			ups++
		} else if strings.Contains(strings.Join(args, " "), " down ") {
			downs++
		}
		return nil
	})
	if err := m.provision(t.Context(), instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, DiskGiB: 2}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ := m.record("orders")
	name = r.Project + "_data"
	for _, mismatch := range []string{"local|ext4||" + r.Project + "|data", "local|ext4|/dev/openrds/other|" + r.Project + "|data"} {
		inspect = mismatch
		if err := m.change(t.Context(), "orders", "start"); err == nil {
			t.Fatal("mismatched data volume accepted for start")
		}
		if err := m.remove(t.Context(), "orders", false); err == nil {
			t.Fatal("mismatched data volume accepted for delete")
		}
	}
	if ups != 1 || downs != 0 || attaches < 1 {
		t.Fatalf("mismatched volume touched: ups=%d downs=%d attaches=%d", ups, downs, attaches)
	}
	inspect = "local|ext4|/dev/openrds/" + r.Project + "|" + r.Project + "|data"
	if err := m.change(t.Context(), "orders", "start"); err != nil || ups != 2 {
		t.Fatalf("correct persisted quota could not restart: %v", err)
	}
}

func TestResizeUpdatesOnlyAfterTheFilesystemChanges(t *testing.T) {
	var project, state, resized string
	m := testManager(t, func(_ context.Context, env []string, _ io.Reader, out io.Writer, args ...string) error {
		switch args[0] {
		case "compose":
			project = args[2]
			return nil
		case "image", "volume":
			if len(args) > 2 && args[1] == "inspect" && strings.Contains(strings.Join(args, " "), "Mountpoint") {
				fmt.Fprint(out, "/var/lib/docker/volumes/openrds_disk_images/_data")
			}
			return nil
		case "ps":
			if state != "" {
				fmt.Fprintf(out, "%s|%s|Up\n", project, state)
			}
			return nil
		case "run":
			if slices.Contains(args, "resize") {
				if !slices.Contains(args, "--env") || !slices.Contains(args, "OPENRDS_BYTES=3221225472") {
					t.Fatalf("resize did not request 3 GiB: %v", args)
				}
				resized = "called"
				return errors.New("data does not fit")
			}
			return nil
		default:
			return nil
		}
	})
	if err := m.provision(t.Context(), instanceSpec{Name: "orders", Engine: "postgres", Port: 15432, DiskGiB: 2}, nil); err != nil {
		t.Fatal(err)
	}
	// The mock reports no container, which is the stopped/not-provisioned case.
	if err := m.resizeDisk(t.Context(), "orders", 2); err != nil {
		t.Fatal("same size should be a no-op:", err)
	}
	state = "running"
	if err := m.resizeDisk(t.Context(), "orders", 3); err == nil || resized != "" {
		t.Fatal("running database accepted a resize")
	}
	state = "stopped"
	if err := m.resizeDisk(t.Context(), "orders", 3); err == nil || resized != "called" {
		t.Fatal("failed resize reported success")
	}
	r, err := m.record("orders")
	if err != nil || r.DiskGiB != 2 {
		t.Fatalf("failed resize changed the stored limit: %+v %v", r, err)
	}
	if err := m.resizeDisk(t.Context(), "plain", 2); err == nil {
		t.Fatal("missing instance resized")
	}
}

func TestRestoreRequiresExplicitUncapOnUnsupportedHost(t *testing.T) {
	var imported string
	m := testManager(t, func(_ context.Context, _ []string, in io.Reader, _ io.Writer, args ...string) error {
		if args[0] == "image" {
			return errors.New("No such image: openrds-disk-helper:local")
		}
		if in != nil {
			b, err := io.ReadAll(in)
			imported = string(b)
			return err
		}
		return nil
	})
	b := backup{ID: randomHex(16), Name: "source", Engine: "postgres", DiskGiB: 20}
	dir := filepath.Join(m.backupDir(), b.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("dump"), 0600); err != nil {
		t.Fatal(err)
	}
	h := handler(m, auth{"test-token"})
	restore := func(body string) int {
		req := httptest.NewRequest("POST", "/api/backups/"+b.ID+"/restore", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := restore(`{"name":"restored","port":15432}`); got != 422 || imported != "" {
		t.Fatalf("inherited cap silently dropped: %d", got)
	}
	if got := restore(`{"name":"restored","port":15432,"diskGiB":0}`); got != 201 || imported != "dump" {
		t.Fatalf("explicit uncapped restore failed: %d", got)
	}
}
