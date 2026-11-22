package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const sessionCookie = "openrds_session"
const sessionLifetime = 12 * time.Hour

func adminToken(dir string) (string, error) {
	path := filepath.Join(dir, "admin-token")
	b, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(b))
		if len(token) != 64 {
			return "", fmt.Errorf("invalid admin token in %s", path)
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token := randomHex(32)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = fmt.Fprintln(f, token)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	return token, closeErr
}

// The API key in the control volume signs sessions, so rotating it signs everyone out.
type auth struct {
	token    string
	accounts *accounts
}

func (a auth) bearer(r *http.Request) bool {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(value), []byte(a.token)) == 1
}

func (a auth) sign(payload string) string {
	h := hmac.New(sha256.New, []byte(a.token))
	h.Write([]byte("openrds-session:" + payload))
	return hex.EncodeToString(h.Sum(nil))
}

// identify returns who made the request. A session cookie is a payload and its
// signature: "t.<expiry>" for the admin token or "u.<id>.<epoch>.<expiry>" for
// an account. A bare "<expiry>" is an admin-token session from before accounts.
func (a auth) identify(r *http.Request) (identity, bool) {
	if a.bearer(r) {
		id := tokenIdentity
		if r.Header.Get("X-OpenRDS-Client") == "command-line" {
			id.Client = "command line"
		}
		return id, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return identity{}, false
	}
	cut := strings.LastIndexByte(c.Value, '.')
	if cut < 0 || !hmac.Equal([]byte(c.Value[cut+1:]), []byte(a.sign(c.Value[:cut]))) {
		return identity{}, false
	}
	fields := strings.Split(c.Value[:cut], ".")
	live := func(expiry string) bool {
		unix, err := strconv.ParseInt(expiry, 10, 64)
		return err == nil && unix > time.Now().Unix()
	}
	switch {
	case len(fields) == 1 && live(fields[0]), len(fields) == 2 && fields[0] == "t" && live(fields[1]):
		return tokenIdentity, true
	case len(fields) == 4 && fields[0] == "u" && live(fields[3]):
		// Disabling an account, resetting it or changing its password raises
		// the epoch, which ends sessions issued before.
		if u, ok := a.accounts.find(fields[1]); ok && u.active() && strconv.Itoa(u.Epoch) == fields[2] {
			return identityOf(u), true
		}
	}
	return identity{}, false
}

// startSession signs in the browser as an account, or as the admin token when u is nil.
func (a auth) startSession(w http.ResponseWriter, r *http.Request, u *user) {
	expiry := strconv.FormatInt(time.Now().Add(sessionLifetime).Unix(), 10)
	payload := "t." + expiry
	if u != nil {
		payload = fmt.Sprintf("u.%s.%d.%s", u.ID, u.Epoch, expiry)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: payload + "." + a.sign(payload), Path: "/api/",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds())})
}

func (a auth) endSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/api/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

var roleNames = map[int]string{roleViewer: "Viewer", roleOperator: "Operator", roleAdmin: "Admin"}

// require admits a request only if its identity has at least the given role.
func require(level int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, _ := identityFrom(r.Context()); id.level() < level {
			sendError(w, fail(403, fmt.Sprintf("This needs the %s role. Ask an administrator for access.", roleNames[level])))
			return
		}
		next.ServeHTTP(w, r)
	})
}
