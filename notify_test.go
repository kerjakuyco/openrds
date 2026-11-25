package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A webhook receiver that remembers every message.
type receiver struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
	status int
}

func newReceiver(t *testing.T) (*receiver, *httptest.Server) {
	rc := &receiver{status: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		rc.mu.Lock()
		rc.paths = append(rc.paths, r.URL.Path)
		rc.bodies = append(rc.bodies, body)
		status := rc.status
		rc.mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, `{"ok":false,"description":"chat not found"}`)
	}))
	t.Cleanup(srv.Close)
	return rc, srv
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func (rc *receiver) last() map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.bodies[len(rc.bodies)-1]
}

func TestChannelsAreCheckedBeforeTheyAreSaved(t *testing.T) {
	refused := []notifyChannel{
		{Kind: "pager", Name: "x", Categories: []string{"health"}},
		{Kind: "slack", Name: "ops", URL: "ftp://hooks.example.com/x", Categories: []string{"health"}},
		{Kind: "slack", Name: "ops", URL: "https://user:pw@hooks.example.com/x", Categories: []string{"health"}},
		{Kind: "slack", Name: "", URL: "https://hooks.example.com/x", Categories: []string{"health"}},
		{Kind: "slack", Name: "ops\nBcc: x", URL: "https://hooks.example.com/x", Categories: []string{"health"}},
		{Kind: "slack", Name: "ops", URL: "https://hooks.example.com/x"},
		{Kind: "slack", Name: "ops", URL: "https://hooks.example.com/x", Categories: []string{"billing"}},
		{Kind: "telegram", Name: "ops", Token: "not-a-token", ChatID: "-100123", Categories: []string{"health"}},
		{Kind: "telegram", Name: "ops", Token: "123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ChatID: "anyone", Categories: []string{"health"}},
		{Kind: "email", Name: "ops", To: []string{"ops@example.com"}, Categories: []string{"health"}}, // no SMTP server yet
	}
	for _, in := range refused {
		if _, err := checkChannel(in, nil, notifyConfig{}); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	withSMTP := notifyConfig{SMTP: &smtpConfig{Host: "smtp.example.com", Port: 587, From: "a@example.com"}}
	ch, err := checkChannel(notifyChannel{Kind: "email", Name: "On call", To: []string{" Ana <ana@example.com>", "ana@example.com", "", "bo@example.com"},
		Categories: []string{"disk", "health", "health"}}, nil, withSMTP)
	if err != nil || strings.Join(ch.To, ",") != "ana@example.com,bo@example.com" || strings.Join(ch.Categories, ",") != "health,disk" {
		t.Fatalf("email channel: %+v %v", ch, err)
	}
	if _, err := checkChannel(notifyChannel{Kind: "email", Name: "x", To: []string{"not an address"}, Categories: []string{"health"}}, nil, withSMTP); status(err) != 400 {
		t.Fatal("a bad address must be refused")
	}
	saved := notifyChannel{ID: "abc", Kind: "slack", Name: "ops", URL: "https://hooks.example.com/services/SECRET", Categories: []string{"health"}}
	edited, err := checkChannel(notifyChannel{Kind: "discord", Name: "renamed", Categories: []string{"failures"}}, &saved, notifyConfig{})
	if err != nil || edited.Kind != "slack" || edited.URL != saved.URL || edited.ID != "abc" {
		t.Fatalf("an edit keeps the kind and, when left empty, the URL: %+v %v", edited, err)
	}

	if _, err := checkSMTP(smtpConfig{Host: "smtp.example.com", Security: "none", Username: "ana", Password: "pw", From: "a@example.com"}, nil); status(err) != 400 {
		t.Fatal("a password must not travel over an unencrypted connection")
	}
	if _, err := checkSMTP(smtpConfig{Host: "smtp.example.com\r\nRCPT", From: "a@example.com"}, nil); status(err) != 400 {
		t.Fatal("a host name with a line break must be refused")
	}
	kept, err := checkSMTP(smtpConfig{Host: "smtp.example.com", Username: "ana", From: "OpenRDS <a@example.com>"}, &smtpConfig{Password: "saved"})
	if err != nil || kept.Password != "saved" || kept.Port != 587 || kept.Security != "starttls" || kept.From != `"OpenRDS" <a@example.com>` {
		t.Fatalf("defaults and the saved password: %+v %v", kept, err)
	}
}

func TestEachChannelKindReceivesItsOwnMessage(t *testing.T) {
	rc, srv := newReceiver(t)
	defer func(api string) { telegramAPI = api }(telegramAPI)
	telegramAPI = srv.URL
	m := testManager(t, newFakeDocker().run)
	ctx := t.Context()
	add := func(ch notifyChannel) notifyChannel {
		t.Helper()
		ch.Categories = []string{categoryHealth, categoryFailures, categoryDisk}
		saved, err := m.addChannel(ctx, ch)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	add(notifyChannel{Kind: "slack", Name: "#ops", URL: srv.URL + "/services/T0/B0/SECRET"})
	if text, _ := rc.last()["text"].(string); !strings.Contains(text, "Test message from OpenRDS") {
		t.Fatalf("adding a channel must send a test message first: %v", rc.last())
	}
	add(notifyChannel{Kind: "discord", Name: "ops", URL: srv.URL + "/api/webhooks/1/SECRET"})
	if _, ok := rc.last()["content"]; !ok {
		t.Fatal("Discord takes content")
	}
	add(notifyChannel{Kind: "telegram", Name: "ops", Token: "123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ChatID: "-1001234567890"})
	if rc.paths[len(rc.paths)-1] != "/bot123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/sendMessage" || rc.last()["chat_id"] != "-1001234567890" {
		t.Fatalf("Telegram: %v %v", rc.paths, rc.last())
	}
	add(notifyChannel{Kind: "webhook", Name: "pager", URL: srv.URL + "/hook"})
	if rc.last()["source"] != "openrds" || rc.last()["category"] != "test" || rc.last()["text"] == "" {
		t.Fatalf("webhook payload: %v", rc.last())
	}

	before := rc.count()
	m.notify(alert{Category: categoryHealth, Severity: "critical", Database: "orders", Title: "orders is unhealthy", Detail: "<b>&</b>"})
	m.alerts.pending.Wait()
	if rc.count() != before+4 {
		t.Fatalf("every channel must receive the alert: %d", rc.count()-before)
	}
	for _, b := range rc.bodies[before:] {
		if text, ok := b["text"].(string); ok && strings.Contains(text, "<b>") && b["source"] == nil && b["chat_id"] == nil {
			t.Error("Slack text must be escaped")
		}
	}

	// Secrets never leave through the API or an error message.
	v, err := m.notifyView()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v)
	for _, secret := range []string{"SECRET", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "/hook"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("the view shows %q: %s", secret, data)
		}
	}
	rc.mu.Lock()
	rc.status = 404
	rc.mu.Unlock()
	err = m.testChannel(ctx, v.Channels[0].ID)
	if status(err) != 502 || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a failed test must say why without the URL: %v", err)
	}
	if v, _ := m.notifyView(); v.Channels[0].LastError == "" || strings.Contains(v.Channels[0].LastError, "SECRET") {
		t.Fatalf("the channel must show its last failure: %+v", v.Channels[0])
	}
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "slack", Name: "typo", URL: srv.URL + "/wrong", Categories: []string{"health"}}); status(err) != 502 {
		t.Fatal("a channel whose test fails must not be saved")
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "slack", Name: "gone", URL: dead.URL + "/services/SECRET", Categories: []string{"health"}}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("an unreachable URL must fail without repeating it: %v", err)
	}
	if v, _ := m.notifyView(); len(v.Channels) != 4 {
		t.Fatalf("channels: %d", len(v.Channels))
	}
}

func TestFailuresAreSentOnceAndOnlyWhereWanted(t *testing.T) {
	rc, srv := newReceiver(t)
	m := testManager(t, newFakeDocker().run)
	ctx := t.Context()
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "webhook", Name: "failures", URL: srv.URL + "/f", Categories: []string{categoryFailures}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "webhook", Name: "health only", URL: srv.URL + "/h", Categories: []string{categoryHealth}}); err != nil {
		t.Fatal(err)
	}
	before := rc.count()
	fail := func(detail string) {
		m.logEvent(ctx, event{Name: "orders", Action: "Copy backup to storage", Result: "failed", Detail: detail})
		m.alerts.pending.Wait()
	}
	fail("bucket unreachable")
	if rc.count() != before+1 || rc.paths[len(rc.paths)-1] != "/f" {
		t.Fatalf("a failure goes only to channels that want failures: %v", rc.paths[before:])
	}
	if got := rc.last(); got["title"] != "Copy backup to storage failed: orders" || got["database"] != "orders" || got["severity"] != "warning" {
		t.Fatalf("failure alert: %v", got)
	}
	fail("bucket unreachable") // retried 10 minutes later
	if rc.count() != before+1 {
		t.Fatal("the same failure must not be sent again within hours")
	}
	fail("access denied")
	if rc.count() != before+2 {
		t.Fatal("a different failure is news")
	}
	m.logEvent(ctx, event{Name: "orders", Action: "Snapshot", Result: "completed"})
	m.raise(ctx, event{Name: "orders", Action: "Health check failing", Result: "failed"}, alert{Category: categoryHealth, Title: "orders is unhealthy"})
	m.alerts.pending.Wait()
	if rc.count() != before+3 || rc.paths[len(rc.paths)-1] != "/h" {
		t.Fatalf("a health alert is sent once, to health channels: %v", rc.paths[before:])
	}
	items, _ := m.readActivity()
	if items[0].Action != "Health check failing" || items[0].Result != "failed" {
		t.Fatal("health changes must show in Events")
	}
}

func TestHealthChangesAreAlerted(t *testing.T) {
	rc, srv := newReceiver(t)
	var mu sync.Mutex
	container := struct {
		id, state, status string
		restarts          int
		oom               bool
		exit              int
	}{"c1", "running", "Up 1 hour (healthy)", 0, false, 0}
	set := func(f func()) { mu.Lock(); f(); mu.Unlock() }
	var project string
	m := testManager(t, func(_ context.Context, _ []string, _ io.Reader, out io.Writer, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "ps":
			if strings.Contains(strings.Join(args, " "), "--no-trunc") {
				fmt.Fprintf(out, "%s|%s|%s|%s\n", container.id, project, container.state, container.status)
			}
		case "inspect":
			fmt.Fprintf(out, "%s|%d|%t|%d\n", container.id, container.restarts, container.oom, container.exit)
		}
		return nil
	})
	r := record{instanceSpec: instanceSpec{Name: "orders", Engine: "postgres", MemoryMiB: 512}, Project: "openrds-db-orders"}
	project = r.Project
	if err := writeJSON(m.recordPath(r.Name), r); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "webhook", Name: "ops", URL: srv.URL, Categories: []string{categoryHealth}}); err != nil {
		t.Fatal(err)
	}
	sent := rc.count()
	check := func(want string) {
		t.Helper()
		m.watchHealth(ctx)
		m.alerts.pending.Wait()
		switch {
		case want == "" && rc.count() != sent:
			t.Fatalf("unexpected alert: %v", rc.last()["title"])
		case want != "" && (rc.count() != sent+1 || !strings.Contains(rc.last()["title"].(string), want)):
			t.Fatalf("want %q, got %d new: %v", want, rc.count()-sent, rc.last()["title"])
		}
		sent = rc.count()
	}
	check("") // the first check only takes note
	check("")
	set(func() { container.restarts, container.oom = 1, true })
	check("orders ran out of memory and restarted")
	if d := rc.last()["detail"].(string); !strings.Contains(d, "512 MiB") || !strings.Contains(d, "running again") {
		t.Fatalf("out-of-memory detail: %s", d)
	}
	check("")
	set(func() { container.status = "Up 2 hours (unhealthy)" })
	check("orders is unhealthy")
	check("")
	set(func() { container.status = "Up 2 hours (healthy)" })
	check("orders is healthy again")
	set(func() {
		container.state, container.status, container.exit = "exited", "Exited (137) 5 seconds ago", 137
	})
	check("orders stopped unexpectedly")
	if d := rc.last()["detail"].(string); !strings.Contains(d, "137") {
		t.Fatalf("exit code: %s", d)
	}

	// An operation, such as a reboot that recreates the container, is not an alert.
	m.mu.Lock()
	m.busy["orders"] = true
	m.mu.Unlock()
	set(func() {
		container.id, container.state, container.status, container.restarts, container.exit = "c2", "running", "Up 1 second (health: starting)", 0, 0
	})
	check("")
	m.mu.Lock()
	delete(m.busy, "orders")
	m.mu.Unlock()
	set(func() { container.status = "Up 1 minute (healthy)" })
	check("orders is healthy again") // the earlier stop was never resolved
	set(func() { container.state, container.status = "restarting", "Restarting (1) 2 seconds ago" })
	check("orders keeps crashing")
	set(func() { container.state, container.status = "exited", "Exited (0) 1 second ago" }) // stopped on purpose
	check("")
	// A user stop (exit 0) after a crash loop leaves the crash unresolved until it runs well again.
	set(func() { container.state, container.status = "running", "Up 5 seconds (healthy)" })
	check("orders is healthy again")
	items, _ := m.readActivity()
	if items[0].Action != "Healthy again" || items[0].Result != "completed" || items[0].Name != "orders" {
		t.Fatalf("events: %+v", items[0])
	}
}

func TestLowDiskIsAlertedOnceAndOnRecovery(t *testing.T) {
	rc, srv := newReceiver(t)
	m := testManager(t, newFakeDocker().run)
	ctx := t.Context()
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "webhook", Name: "ops", URL: srv.URL, Categories: []string{categoryDisk}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	free := uint64(50 << 30)
	m.space = func(string) (uint64, uint64, bool) { mu.Lock(); defer mu.Unlock(); return free, 100 << 30, true } // reserve 5 GiB
	sent := rc.count()
	step := func(gib uint64, want string) {
		t.Helper()
		mu.Lock()
		free = gib << 30
		mu.Unlock()
		m.watchDisk(ctx)
		m.alerts.pending.Wait()
		if want == "" && rc.count() != sent || want != "" && (rc.count() != sent+1 || rc.last()["title"] != want) {
			t.Fatalf("at %d GiB want %q, got %d new: %v", gib, want, rc.count()-sent, rc.last()["title"])
		}
		sent = rc.count()
	}
	step(50, "")
	step(8, "Disk space is running low")
	step(7, "")
	step(4, "Backups are paused: the disk is almost full")
	step(3, "")
	step(9, "")
	step(20, "Disk space is back to normal")
}

// A minimal SMTP server that accepts one message.
func fakeSMTP(t *testing.T) (port int, got func() string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var data strings.Builder
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				fmt.Fprint(c, "220 fake ESMTP\r\n")
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							fmt.Fprint(c, "250 queued\r\n")
							continue
						}
						mu.Lock()
						data.WriteString(line)
						mu.Unlock()
						continue
					}
					switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						fmt.Fprint(c, "250 fake\r\n")
					case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
						fmt.Fprint(c, "250 ok\r\n")
					case cmd == "DATA":
						inData = true
						fmt.Fprint(c, "354 go on\r\n")
					case cmd == "QUIT":
						fmt.Fprint(c, "221 bye\r\n")
						return
					default:
						fmt.Fprint(c, "502 no\r\n")
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() string { mu.Lock(); defer mu.Unlock(); return data.String() }
}

func TestEmailAlertsGoThroughTheSMTPServer(t *testing.T) {
	port, got := fakeSMTP(t)
	m := testManager(t, newFakeDocker().run)
	ctx := t.Context()
	if err := m.saveSMTP(ctx, smtpConfig{Host: "127.0.0.1", Port: port, Security: "starttls", From: "alerts@example.com"}); status(err) != 502 || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("a server without STARTTLS must be reported: %v", err)
	}
	if err := m.saveSMTP(ctx, smtpConfig{Host: "127.0.0.1", Port: port, Security: "none", From: "OpenRDS <alerts@example.com>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.addChannel(ctx, notifyChannel{Kind: "email", Name: "On call", To: []string{"ana@example.com"}, Categories: []string{categoryHealth}}); err != nil {
		t.Fatal(err)
	}
	m.notify(alert{Category: categoryHealth, Severity: "critical", Database: "orders", Title: "orders ran out of memory and restarted", Detail: "It used all of its 512 MiB memory limit."})
	m.alerts.pending.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got(), "ran out of memory") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	msg := got()
	for _, want := range []string{"To: ana@example.com", "Subject: [OpenRDS ", "orders ran out of memory and restarted", "512 MiB", "Content-Transfer-Encoding: quoted-printable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, msg)
		}
	}
	if err := m.removeSMTP(ctx); status(err) != 409 {
		t.Fatal("the server must stay while email channels use it")
	}
}

func TestOnlyAdminsManageNotifications(t *testing.T) {
	ws := newWorkspace(t)
	ws.person(t, "ada", "admin")
	ws.person(t, "olga", "operator")
	ws.person(t, "vic", "viewer")
	admin, operator, viewer := ws.signIn(t, "ada"), ws.signIn(t, "olga"), ws.signIn(t, "vic")
	for _, c := range []struct {
		method, path, body string
	}{
		{"GET", "/api/notifications", ""},
		{"PUT", "/api/notifications/smtp", "{}"},
		{"POST", "/api/notifications/channels", "{}"},
		{"POST", "/api/notifications/channels/x/test", ""},
		{"DELETE", "/api/notifications/channels/x", ""},
	} {
		for _, who := range []*client{operator, viewer} {
			if code, _ := who.do(c.method, c.path, c.body); code != 403 {
				t.Errorf("%s %s by a non-admin: %d", c.method, c.path, code)
			}
		}
	}
	if code, body := admin.do("GET", "/api/notifications", ""); code != 200 || !strings.Contains(body, `"channels":[]`) {
		t.Fatalf("admin: %d %s", code, body)
	}
	if code, _ := admin.do("POST", "/api/notifications/channels", `{"kind":"slack","name":"ops","url":"ftp://x","categories":["health"]}`); code != 400 {
		t.Fatalf("a bad channel: %d", code)
	}
}
