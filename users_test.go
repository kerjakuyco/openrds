package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func init() { passwordIterations = 1000 } // keep tests fast; production uses OWASP's count

func testAuth(t *testing.T, m *manager, token string) auth {
	t.Helper()
	people, err := openAccounts(filepath.Join(m.dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	return auth{token: token, accounts: people}
}

// One browser: it keeps whatever session cookie the server sets.
type client struct {
	h      http.Handler
	cookie *http.Cookie
	bearer string
}

func (c *client) do(method, path, body string) (int, string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	for _, k := range w.Result().Cookies() {
		if k.Name == sessionCookie {
			if k.MaxAge < 0 {
				c.cookie = nil
			} else {
				c.cookie = k
			}
		}
	}
	return w.Code, w.Body.String()
}

type workspace struct {
	m     *manager
	a     auth
	h     http.Handler
	clock time.Time
}

func newWorkspace(t *testing.T) *workspace {
	m := testManager(t, newFakeDocker().run)
	a := testAuth(t, m, "test-token")
	ws := &workspace{m: m, a: a, h: handler(m, a), clock: time.Now()}
	a.accounts.now = func() time.Time { return ws.clock }
	return ws
}

// Create an active account with a password, the way an invited person would.
func (ws *workspace) person(t *testing.T, username, role string) user {
	t.Helper()
	u, secret, err := ws.a.accounts.create(username, "", role)
	if err != nil {
		t.Fatal(err)
	}
	if u, err = ws.a.accounts.redeem(secret, username+"-password"); err != nil {
		t.Fatal(err)
	}
	return u
}

func (ws *workspace) signIn(t *testing.T, username string) *client {
	t.Helper()
	c := &client{h: ws.h}
	if code, body := c.do("POST", "/api/session", fmt.Sprintf(`{"username":%q,"password":%q}`, username, username+"-password")); code != 200 {
		t.Fatalf("sign in %s: %d %s", username, code, body)
	}
	return c
}

func TestPasswordHashes(t *testing.T) {
	hash, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$1000$") || strings.Contains(hash, "correct") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if !checkPassword(hash, "correct horse battery") || checkPassword(hash, "correct horse battery ") {
		t.Fatal("password check")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$0$AA$AA", "pbkdf2-sha256$1000$!!$AA", strings.Replace(hash, "sha256", "md5", 1)} {
		if checkPassword(bad, "correct horse battery") {
			t.Fatalf("accepted malformed hash %q", bad)
		}
	}
}

func TestFirstRunSetupAndSignIn(t *testing.T) {
	ws := newWorkspace(t)
	anon := &client{h: ws.h}
	if _, body := anon.do("GET", "/api/status", ""); body != "{\"setup\":true}\n" {
		t.Fatalf("status before setup: %s", body)
	}
	form := `{"username":"Ada","name":"Ada Lovelace","password":"%s"}`
	if code, _ := anon.do("POST", "/api/setup", fmt.Sprintf(form, "analytical-engine")); code != 401 {
		t.Fatal("setup must prove access with the admin token")
	}
	admin := &client{h: ws.h, bearer: "test-token"}
	if code, _ := admin.do("POST", "/api/setup", fmt.Sprintf(form, "short")); code != 400 {
		t.Fatal("weak password accepted")
	}
	if code, body := admin.do("POST", "/api/setup", fmt.Sprintf(form, "analytical-engine")); code != 201 || admin.cookie == nil {
		t.Fatalf("setup: %d %s", code, body)
	}
	admin.bearer = ""
	_, body := admin.do("GET", "/api/session", "")
	var me identity
	if json.Unmarshal([]byte(body), &me); me.Username != "ada" || me.Role != "admin" || me.Token {
		t.Fatalf("identity after setup: %s", body)
	}
	if code, _ := (&client{h: ws.h, bearer: "test-token"}).do("POST", "/api/setup", fmt.Sprintf(form, "analytical-engine")); code != 409 {
		t.Fatal("a second setup was accepted")
	}
	if _, body := anon.do("GET", "/api/status", ""); body != "{\"setup\":false}\n" {
		t.Fatal("status after setup")
	}

	browser := &client{h: ws.h}
	for range lockoutAfter {
		if code, _ := browser.do("POST", "/api/session", `{"username":"ada","password":"wrong-password"}`); code != 401 {
			t.Fatal("wrong password accepted")
		}
	}
	if code, body := browser.do("POST", "/api/session", `{"username":"ada","password":"analytical-engine"}`); code != 429 || !strings.Contains(body, "Try again") {
		t.Fatalf("no lockout after repeated failures: %d %s", code, body)
	}
	ws.clock = ws.clock.Add(31 * time.Second)
	if code, _ := browser.do("POST", "/api/session", `{"username":"ADA","password":"analytical-engine"}`); code != 200 {
		t.Fatal("lockout did not expire, or usernames are case-sensitive")
	}
	items, _ := ws.m.readActivity()
	if len(items) == 0 || items[len(items)-1].Action != "Create administrator account" || items[len(items)-1].Actor != "ada" || items[len(items)-1].Kind != "user" {
		t.Fatalf("setup was not recorded: %+v", items)
	}
}

func TestRolesAreEnforced(t *testing.T) {
	ws := newWorkspace(t)
	ws.person(t, "ada", "admin")
	ws.person(t, "olga", "operator")
	ws.person(t, "vic", "viewer")
	admin, operator, viewer := ws.signIn(t, "ada"), ws.signIn(t, "olga"), ws.signIn(t, "vic")
	create := `{"name":"orders","engine":"postgres","port":15432}`
	checks := []struct {
		c            *client
		method, path string
		body         string
		want         int
	}{
		{viewer, "GET", "/api/instances", "", 200},
		{viewer, "GET", "/api/activity", "", 200},
		{viewer, "POST", "/api/instances", create, 403},
		{operator, "POST", "/api/instances", create, 201},
		{viewer, "GET", "/api/instances/orders/connection", "", 403},
		{operator, "GET", "/api/instances/orders/connection", "", 200},
		{viewer, "POST", "/api/instances/orders/backups", "", 403},
		{viewer, "GET", "/api/users", "", 403},
		{operator, "GET", "/api/users", "", 403},
		{operator, "POST", "/api/users", `{"username":"eve","role":"admin"}`, 403},
		{admin, "GET", "/api/users", "", 200},
	}
	for _, c := range checks {
		if code, body := c.c.do(c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s: got %d, want %d (%s)", c.method, c.path, code, c.want, body)
		}
	}
	if _, body := admin.do("GET", "/api/users", ""); strings.Contains(body, "pbkdf2") || strings.Contains(body, "hash") || strings.Contains(body, "password") {
		t.Fatalf("account list leaks secrets: %s", body)
	}
	items, _ := ws.m.readActivity()
	if !slices.ContainsFunc(items, func(e event) bool { return e.Action == "Provision database" && e.Actor == "olga" }) {
		t.Fatalf("operator not recorded as the actor: %+v", items)
	}
}

func TestInviteAndResetLinks(t *testing.T) {
	ws := newWorkspace(t)
	ws.person(t, "ada", "admin")
	admin := ws.signIn(t, "ada")
	code, body := admin.do("POST", "/api/users", `{"username":"bob","name":"Bob","role":"operator"}`)
	var created struct {
		User   userView `json:"user"`
		Secret string   `json:"secret"`
	}
	if json.Unmarshal([]byte(body), &created); code != 201 || created.User.Status != "invited" || len(created.Secret) != 64 {
		t.Fatalf("invite: %d %s", code, body)
	}
	bob := &client{h: ws.h}
	if code, body := bob.do("GET", "/api/invites/"+created.Secret, ""); code != 200 || !strings.Contains(body, `"username":"bob"`) {
		t.Fatalf("invite info: %d %s", code, body)
	}
	if code, _ := bob.do("GET", "/api/invites/"+strings.Repeat("0", 64), ""); code != 404 {
		t.Fatal("unknown link accepted")
	}
	if code, _ := bob.do("POST", "/api/invites/"+created.Secret, `{"password":"bob"}`); code != 400 {
		t.Fatal("weak password accepted from an invite")
	}
	if code, _ := bob.do("POST", "/api/invites/"+created.Secret, `{"password":"bob-password"}`); code != 200 || bob.cookie == nil {
		t.Fatal("invite was not redeemed")
	}
	if code, _ := (&client{h: ws.h}).do("POST", "/api/invites/"+created.Secret, `{"password":"another-password"}`); code != 404 {
		t.Fatal("an invite link worked twice")
	}
	if code, _ := bob.do("GET", "/api/instances", ""); code != 200 {
		t.Fatal("invited user could not use the session")
	}

	code, body = admin.do("POST", "/api/users/"+created.User.ID+"/reset", "")
	var reset struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal([]byte(body), &reset); code != 200 || reset.Secret == "" {
		t.Fatalf("reset: %d %s", code, body)
	}
	if code, _ := bob.do("GET", "/api/instances", ""); code != 401 {
		t.Fatal("a reset did not end the existing session")
	}
	if code, _ := (&client{h: ws.h}).do("POST", "/api/session", `{"username":"bob","password":"bob-password"}`); code != 401 {
		t.Fatal("the old password still works after a reset")
	}

	_, secret, err := ws.a.accounts.create("carol", "", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	ws.clock = ws.clock.Add(inviteLifetime + time.Minute)
	if code, _ := (&client{h: ws.h}).do("GET", "/api/invites/"+secret, ""); code != 404 {
		t.Fatal("an expired invite was accepted")
	}
}

func TestAccountsProtectThemselves(t *testing.T) {
	ws := newWorkspace(t)
	ada := ws.person(t, "ada", "admin")
	bob := ws.person(t, "bob", "operator")
	admin := ws.signIn(t, "ada")
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/api/users/" + ada.ID, `{"role":"viewer"}`},
		{"PATCH", "/api/users/" + ada.ID, `{"disabled":true}`},
		{"POST", "/api/users/" + ada.ID + "/reset", ""},
		{"DELETE", "/api/users/" + ada.ID, ""},
	} {
		if code, _ := admin.do(c.method, c.path, c.body); code != 409 {
			t.Errorf("%s %s on yourself: got %d", c.method, c.path, code)
		}
	}
	// The invariant holds even for requests that slip past the self checks.
	viewerActor := identity{ID: bob.ID, Role: "admin"}
	if _, _, err := ws.a.accounts.update(viewerActor, ada.ID, accountChange{Role: ptr("viewer")}); status(err) != 409 {
		t.Fatal("the last active administrator was demoted")
	}
	session := ws.signIn(t, "bob")
	if code, _ := admin.do("PATCH", "/api/users/"+bob.ID, `{"disabled":true}`); code != 200 {
		t.Fatal("could not disable an account")
	}
	if code, _ := session.do("GET", "/api/instances", ""); code != 401 {
		t.Fatal("a disabled account kept its session")
	}
	if code, _ := (&client{h: ws.h}).do("POST", "/api/session", `{"username":"bob","password":"bob-password"}`); code != 401 {
		t.Fatal("a disabled account signed in")
	}
	admin.do("PATCH", "/api/users/"+bob.ID, `{"disabled":false,"role":"viewer"}`)
	if c := ws.signIn(t, "bob"); c == nil {
		t.Fatal("re-enabled account cannot sign in")
	}
	items, _ := ws.m.readActivity()
	if items[0].Action != "Update user" || items[0].Detail != "Role: operator → viewer · Enabled" || items[0].Actor != "ada" {
		t.Fatalf("account change not described: %+v", items[0])
	}
	// The admin token can always repair accounts, even removing the last administrator.
	if code, _ := (&client{h: ws.h, bearer: "test-token"}).do("DELETE", "/api/users/"+ada.ID, ""); code != 204 {
		t.Fatal("the admin token could not remove an account")
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	ws := newWorkspace(t)
	ws.person(t, "ada", "admin")
	laptop, phone := ws.signIn(t, "ada"), ws.signIn(t, "ada")
	if code, _ := laptop.do("POST", "/api/account/password", `{"current":"nope","password":"a-brand-new-password"}`); code != 403 {
		t.Fatal("password changed without the current one")
	}
	if code, _ := laptop.do("POST", "/api/account/password", `{"current":"ada-password","password":"a-brand-new-password"}`); code != 204 {
		t.Fatal("password change failed")
	}
	if code, _ := laptop.do("GET", "/api/session", ""); code != 200 {
		t.Fatal("the browser that changed the password was signed out")
	}
	if code, _ := phone.do("GET", "/api/session", ""); code != 401 {
		t.Fatal("other sessions survived a password change")
	}
	tablet := &client{h: ws.h}
	if code, _ := tablet.do("POST", "/api/session", `{"username":"ada","password":"a-brand-new-password"}`); code != 200 {
		t.Fatal("new password does not work")
	}
	if code, _ := laptop.do("POST", "/api/account/signout", ""); code != 204 {
		t.Fatal("sign out everywhere failed")
	}
	if code, _ := tablet.do("GET", "/api/session", ""); code != 401 {
		t.Fatal("sign out everywhere left a session")
	}
	if code, _ := laptop.do("PATCH", "/api/account", `{"name":"Countess Lovelace"}`); code != 200 {
		t.Fatal("rename failed")
	}
	if code, _ := (&client{h: ws.h, bearer: "test-token"}).do("PATCH", "/api/account", `{"name":"x"}`); code != 409 {
		t.Fatal("the admin token pretended to have an account")
	}
}

func TestSessionsFromBeforeAccountsStillWork(t *testing.T) {
	ws := newWorkspace(t)
	exp := fmt.Sprint(time.Now().Add(time.Hour).Unix())
	legacy := &client{h: ws.h, cookie: &http.Cookie{Name: sessionCookie, Value: exp + "." + ws.a.sign(exp)}}
	code, body := legacy.do("GET", "/api/session", "")
	if code != 200 || !strings.Contains(body, `"token":true`) {
		t.Fatalf("legacy session: %d %s", code, body)
	}
	forged := &client{h: ws.h, cookie: &http.Cookie{Name: sessionCookie, Value: "u.someone.0." + exp + "." + ws.a.sign(exp)}}
	if code, _ := forged.do("GET", "/api/session", ""); code != 401 {
		t.Fatal("a cookie with a mismatched signature was accepted")
	}
	// An admin-token session can create the first account without the raw token.
	if code, body := legacy.do("POST", "/api/setup", `{"username":"ada","password":"analytical-engine"}`); code != 201 || !strings.Contains(body, `"username":"ada"`) {
		t.Fatalf("setup from an admin-token session: %d %s", code, body)
	}
	if code, body := legacy.do("GET", "/api/session", ""); code != 200 || strings.Contains(body, `"token":true`) {
		t.Fatalf("setup did not switch the browser to the new account: %d %s", code, body)
	}
}
