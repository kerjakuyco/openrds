package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	roleViewer = iota + 1
	roleOperator
	roleAdmin
)

var roleLevels = map[string]int{"viewer": roleViewer, "operator": roleOperator, "admin": roleAdmin}

var (
	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)
	secretPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// PBKDF2-HMAC-SHA256 iterations for new passwords, following OWASP guidance.
// Each hash records its own count, so raising this later keeps old hashes valid.
var passwordIterations = 600_000

const (
	inviteLifetime = 72 * time.Hour
	lockoutAfter   = 5
	usernameRule   = "Use a username of 2–32 lowercase letters, digits, dots, hyphens or underscores, starting with a letter or digit."
	linkInvalid    = "This link is not valid. It may have been used or have expired; ask an administrator for a new one."
)

type invite struct {
	Hash    string    `json:"hash"` // SHA-256 of the link secret; the secret itself is never stored
	Expires time.Time `json:"expiresAt"`
	Reset   bool      `json:"reset,omitempty"`
}

type user struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Password  string     `json:"password,omitempty"`
	Epoch     int        `json:"epoch"` // raising it ends every session of the account
	Disabled  bool       `json:"disabled,omitempty"`
	Created   time.Time  `json:"createdAt"`
	LastLogin *time.Time `json:"lastLoginAt,omitempty"`
	Invite    *invite    `json:"invite,omitempty"`
}

func (u user) active() bool { return !u.Disabled && u.Password != "" }

// The API view of an account: never the password hash or the invite secret.
type userView struct {
	ID            string     `json:"id"`
	Username      string     `json:"username"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	Status        string     `json:"status"` // active, invited or disabled
	Created       time.Time  `json:"createdAt"`
	LastLogin     *time.Time `json:"lastLoginAt,omitempty"`
	InviteExpires *time.Time `json:"inviteExpiresAt,omitempty"`
}

func (u user) view() userView {
	v := userView{ID: u.ID, Username: u.Username, Name: u.Name, Role: u.Role, Status: "active", Created: u.Created, LastLogin: u.LastLogin}
	switch {
	case u.Disabled:
		v.Status = "disabled"
	case u.Password == "":
		v.Status = "invited"
	}
	if u.Invite != nil && !u.Disabled {
		expires := u.Invite.Expires
		v.InviteExpires = &expires
	}
	return v
}

// Who is making a request: an account, or the admin token.
type identity struct {
	ID       string `json:"id,omitempty"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Token    bool   `json:"token"`
}

var tokenIdentity = identity{Username: "admin-token", Name: "Admin token", Role: "admin", Token: true}

func identityOf(u user) identity {
	return identity{ID: u.ID, Username: u.Username, Name: u.Name, Role: u.Role}
}

func (i identity) level() int { return roleLevels[i.Role] }

type identityKey struct{}

func withIdentity(ctx context.Context, i identity) context.Context {
	return context.WithValue(ctx, identityKey{}, i)
}

func identityFrom(ctx context.Context) (identity, bool) {
	i, ok := ctx.Value(identityKey{}).(identity)
	return i, ok
}

// The name recorded in events: an account, the admin token, or the scheduler.
func actorFrom(ctx context.Context) string {
	i, ok := identityFrom(ctx)
	switch {
	case !ok:
		return "system"
	case i.Token:
		return "admin token"
	default:
		return i.Username
	}
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func checkPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// Unknown usernames are checked against this hash, so they take as long as wrong passwords.
var dummyHash = sync.OnceValue(func() string {
	h, _ := hashPassword(randomHex(16))
	return h
})

func checkNewPassword(password, username string) error {
	switch n := utf8.RuneCountInString(password); {
	case n < 10:
		return fail(400, "Use a password of at least 10 characters.")
	case n > 128:
		return fail(400, "Use a password of at most 128 characters.")
	case strings.EqualFold(password, username):
		return fail(400, "Choose a password that is not your username.")
	}
	return nil
}

func cleanName(name, username string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = username
	}
	if utf8.RuneCountInString(name) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fail(400, "Use a display name of up to 64 characters.")
	}
	return name, nil
}

func cleanUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return "", fail(400, usernameRule)
	}
	return username, nil
}

func newSecret() (secret, hash string) {
	secret = randomHex(32)
	sum := sha256.Sum256([]byte(secret))
	return secret, hex.EncodeToString(sum[:])
}

type throttle struct {
	failures int
	until    time.Time
}

// Accounts for the console, kept in users.json in the control volume.
type accounts struct {
	mu    sync.Mutex
	path  string
	list  []user
	fails map[string]*throttle // failed sign-ins by username
	now   func() time.Time
}

func openAccounts(path string) (*accounts, error) {
	a := &accounts{path: path, fails: make(map[string]*throttle), now: time.Now}
	err := readJSON(path, &a.list)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return a, err
}

// change applies fn to a copy of the accounts and keeps the result only once it
// is saved, so memory never disagrees with users.json. Callers hold mu.
func (a *accounts) change(fn func([]user) ([]user, error)) error {
	next, err := fn(slices.Clone(a.list))
	if err != nil {
		return err
	}
	if err := writeJSON(a.path, next); err != nil {
		return err
	}
	a.list = next
	return nil
}

func (a *accounts) index(id string) int {
	return slices.IndexFunc(a.list, func(u user) bool { return u.ID == id })
}

func (a *accounts) byUsername(username string) int {
	return slices.IndexFunc(a.list, func(u user) bool { return u.Username == username })
}

func (a *accounts) any() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.list) > 0
}

func (a *accounts) find(id string) (user, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := a.index(id); i >= 0 {
		return a.list[i], true
	}
	return user{}, false
}

func (a *accounts) views() []userView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]userView, 0, len(a.list))
	for _, u := range a.list {
		out = append(out, u.view())
	}
	return out
}

// The first account is an administrator, created only while no account exists.
func (a *accounts) setup(username, name, password string) (user, error) {
	username, err := cleanUsername(username)
	if err != nil {
		return user{}, err
	}
	if name, err = cleanName(name, username); err != nil {
		return user{}, err
	}
	if err := checkNewPassword(password, username); err != nil {
		return user{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return user{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.list) > 0 {
		return user{}, fail(409, "OpenRDS already has accounts. Sign in instead.")
	}
	now := a.now().UTC()
	u := user{ID: randomHex(16), Username: username, Name: name, Role: "admin", Password: hash, Created: now, LastLogin: &now}
	return u, a.change(func(list []user) ([]user, error) { return append(list, u), nil })
}

// Five failures in a row lock a username for 30 seconds, doubling up to 15
// minutes. Unknown usernames are throttled the same way, so they reveal nothing.
func (a *accounts) login(username, password string) (user, error) {
	key := strings.ToLower(strings.TrimSpace(username))
	now := a.now()
	a.mu.Lock()
	if t := a.fails[key]; t != nil && now.Before(t.until) {
		a.mu.Unlock()
		return user{}, fail(429, fmt.Sprintf("Too many failed sign-ins for this account. Try again in %s.", t.until.Sub(now).Round(time.Second)))
	}
	var u user
	found := a.byUsername(key)
	if found >= 0 {
		u = a.list[found]
	}
	a.mu.Unlock()
	stored := u.Password
	if stored == "" {
		stored = dummyHash()
	}
	ok := checkPassword(stored, password) && found >= 0 && u.active()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !ok {
		if len(a.fails) > 10000 {
			clear(a.fails) // bound memory under a flood of made-up usernames
		}
		t := a.fails[key]
		if t == nil {
			t = &throttle{}
			a.fails[key] = t
		}
		t.failures++
		if t.failures >= lockoutAfter {
			t.until = now.Add(min(30*time.Second<<min(t.failures-lockoutAfter, 5), 15*time.Minute))
		}
		return user{}, fail(401, "The username or password is not correct.")
	}
	delete(a.fails, key)
	i := a.index(u.ID)
	if i < 0 {
		return user{}, fail(401, "The username or password is not correct.")
	}
	stamp := now.UTC()
	err := a.change(func(list []user) ([]user, error) {
		list[i].LastLogin = &stamp
		return list, nil
	})
	return a.list[i], err
}

// Admins create an account with a one-time link; the person picks their own password.
func (a *accounts) create(username, name, role string) (user, string, error) {
	username, err := cleanUsername(username)
	if err != nil {
		return user{}, "", err
	}
	if name, err = cleanName(name, username); err != nil {
		return user{}, "", err
	}
	if _, ok := roleLevels[role]; !ok {
		return user{}, "", fail(400, "Choose the admin, operator or viewer role.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byUsername(username) >= 0 {
		return user{}, "", fail(409, "That username is already taken.")
	}
	secret, hash := newSecret()
	now := a.now().UTC()
	u := user{ID: randomHex(16), Username: username, Name: name, Role: role, Created: now,
		Invite: &invite{Hash: hash, Expires: now.Add(inviteLifetime)}}
	return u, secret, a.change(func(list []user) ([]user, error) { return append(list, u), nil })
}

// Callers hold mu.
func (a *accounts) invited(secret string) (int, error) {
	if !secretPattern.MatchString(secret) {
		return -1, fail(404, linkInvalid)
	}
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	now := a.now()
	for i, u := range a.list {
		if u.Invite != nil && !u.Disabled && subtle.ConstantTimeCompare([]byte(hash), []byte(u.Invite.Hash)) == 1 && now.Before(u.Invite.Expires) {
			return i, nil
		}
	}
	return -1, fail(404, linkInvalid)
}

func (a *accounts) inviteInfo(secret string) (user, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i, err := a.invited(secret)
	if err != nil {
		return user{}, err
	}
	return a.list[i], nil
}

// Set the password from an invite or reset link. The link works once.
func (a *accounts) redeem(secret, password string) (user, error) {
	u, err := a.inviteInfo(secret)
	if err != nil {
		return user{}, err
	}
	if err := checkNewPassword(password, u.Username); err != nil {
		return user{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return user{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i, err := a.invited(secret) // still unused while the hash was computed
	if err != nil || a.list[i].ID != u.ID {
		return user{}, fail(404, linkInvalid)
	}
	now := a.now().UTC()
	err = a.change(func(list []user) ([]user, error) {
		list[i].Password, list[i].Invite, list[i].LastLogin = hash, nil, &now
		list[i].Epoch++
		return list, nil
	})
	return a.list[i], err
}

// A user cannot leave OpenRDS without an active administrator. The admin token
// is exempt: it can always sign in and repair accounts. Callers hold mu.
func (a *accounts) keepAdmin(actor identity, i int, next *user) error {
	if actor.Token {
		return nil
	}
	for j, u := range a.list {
		if j == i {
			if next != nil && next.Role == "admin" && next.active() {
				return nil
			}
			continue
		}
		if u.Role == "admin" && u.active() {
			return nil
		}
	}
	return fail(409, "OpenRDS needs at least one active administrator. Make another account an administrator first.")
}

type accountChange struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
}

func (a *accounts) update(actor identity, id string, c accountChange) (before, after user, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.index(id)
	if i < 0 {
		return before, after, fail(404, "Account not found.")
	}
	before = a.list[i]
	next := before
	if c.Name != nil {
		if next.Name, err = cleanName(*c.Name, next.Username); err != nil {
			return before, after, err
		}
	}
	if c.Role != nil {
		if _, ok := roleLevels[*c.Role]; !ok {
			return before, after, fail(400, "Choose the admin, operator or viewer role.")
		}
		next.Role = *c.Role
	}
	if c.Disabled != nil {
		next.Disabled = *c.Disabled
	}
	if !actor.Token && actor.ID == id && (next.Role != before.Role || next.Disabled) {
		return before, after, fail(409, "You cannot change your own role or disable your own account. Ask another administrator.")
	}
	if err := a.keepAdmin(actor, i, &next); err != nil {
		return before, after, err
	}
	if next.Disabled && !before.Disabled {
		next.Epoch++ // end the account's sessions now
	}
	err = a.change(func(list []user) ([]user, error) {
		list[i] = next
		return list, nil
	})
	return before, next, err
}

// A reset ends the account's sessions and issues a new one-time link.
func (a *accounts) reset(actor identity, id string) (user, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.index(id)
	if i < 0 {
		return user{}, "", fail(404, "Account not found.")
	}
	if !actor.Token && actor.ID == id {
		return user{}, "", fail(409, "Change your own password from Account settings.")
	}
	next := a.list[i]
	secret, hash := newSecret()
	next.Password, next.Epoch = "", next.Epoch+1
	next.Invite = &invite{Hash: hash, Expires: a.now().UTC().Add(inviteLifetime), Reset: true}
	if err := a.keepAdmin(actor, i, &next); err != nil {
		return user{}, "", err
	}
	return next, secret, a.change(func(list []user) ([]user, error) {
		list[i] = next
		return list, nil
	})
}

func (a *accounts) remove(actor identity, id string) (user, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.index(id)
	if i < 0 {
		return user{}, fail(404, "Account not found.")
	}
	if !actor.Token && actor.ID == id {
		return user{}, fail(409, "You cannot delete your own account.")
	}
	if err := a.keepAdmin(actor, i, nil); err != nil {
		return user{}, err
	}
	gone := a.list[i]
	return gone, a.change(func(list []user) ([]user, error) { return slices.Delete(list, i, i+1), nil })
}

func (a *accounts) rename(id, name string) (user, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.index(id)
	if i < 0 {
		return user{}, fail(404, "Account not found.")
	}
	clean, err := cleanName(name, a.list[i].Username)
	if err != nil {
		return user{}, err
	}
	err = a.change(func(list []user) ([]user, error) {
		list[i].Name = clean
		return list, nil
	})
	return a.list[i], err
}

// A new password, or "sign out everywhere" with an empty one, ends every session
// of the account; the caller then starts a fresh one for the current browser.
func (a *accounts) changePassword(id, current, next string) (user, error) {
	a.mu.Lock()
	i := a.index(id)
	var u user
	if i >= 0 {
		u = a.list[i]
	}
	a.mu.Unlock()
	if i < 0 {
		return user{}, fail(404, "Account not found.")
	}
	if !checkPassword(u.Password, current) {
		return user{}, fail(403, "The current password is not correct.")
	}
	if err := checkNewPassword(next, u.Username); err != nil {
		return user{}, err
	}
	hash, err := hashPassword(next)
	if err != nil {
		return user{}, err
	}
	return a.bump(id, hash)
}

func (a *accounts) bump(id, hash string) (user, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.index(id)
	if i < 0 {
		return user{}, fail(404, "Account not found.")
	}
	err := a.change(func(list []user) ([]user, error) {
		if hash != "" {
			list[i].Password = hash
		}
		list[i].Epoch++
		return list, nil
	})
	return a.list[i], err
}
