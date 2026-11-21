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

type auth struct{ token string }

func (a auth) bearer(r *http.Request) bool {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(value), []byte(a.token)) == 1
}

func (a auth) sign(expiry string) string {
	h := hmac.New(sha256.New, []byte(a.token))
	h.Write([]byte("openrds-session:" + expiry))
	return hex.EncodeToString(h.Sum(nil))
}

func (a auth) authorized(r *http.Request) bool {
	if a.bearer(r) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	unix, err := strconv.ParseInt(exp, 10, 64)
	return ok && err == nil && unix > time.Now().Unix() && hmac.Equal([]byte(sig), []byte(a.sign(exp)))
}

func (a auth) cookie(w http.ResponseWriter, r *http.Request, login bool) {
	c := &http.Cookie{Name: sessionCookie, Path: "/api/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: -1}
	if login {
		c.MaxAge = int(sessionLifetime.Seconds())
		exp := strconv.FormatInt(time.Now().Add(sessionLifetime).Unix(), 10)
		c.Value = exp + "." + a.sign(exp)
	}
	http.SetCookie(w, c)
}
