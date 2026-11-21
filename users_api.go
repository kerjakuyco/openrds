package main

import (
	"net/http"
	"strings"
)

func accountOf(r *http.Request) (identity, error) {
	id, _ := identityFrom(r.Context())
	if id.Token {
		return id, fail(409, "You are signed in with the admin token, which has no account. Sign in with an account to change its settings.")
	}
	return id, nil
}

func describeChange(before, after user) string {
	var parts []string
	if before.Role != after.Role {
		parts = append(parts, "Role: "+before.Role+" → "+after.Role)
	}
	if before.Disabled != after.Disabled {
		parts = append(parts, map[bool]string{true: "Disabled", false: "Enabled"}[after.Disabled])
	}
	if before.Name != after.Name {
		parts = append(parts, "Display name: "+after.Name)
	}
	return strings.Join(parts, " · ")
}

// Sign-in, first-run setup, invitations and account management.
func accountRoutes(mux *http.ServeMux, add func(string, int, endpoint), m *manager, a auth) {
	// Public: what the sign-in page needs before anyone is signed in.
	mux.Handle("GET /api/status", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		sendJSON(w, 200, map[string]bool{"setup": !a.accounts.any()})
		return nil
	}))
	// The first account needs the admin token: in the request, or as the
	// browser's current admin-token session.
	mux.Handle("POST /api/setup", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		if id, ok := a.identify(r); !ok || !id.Token {
			return fail(401, "The admin token was not accepted.")
		}
		var body struct {
			Username string `json:"username"`
			Name     string `json:"name"`
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		u, err := a.accounts.setup(body.Username, body.Name, body.Password)
		if err != nil {
			return err
		}
		m.noteUser(withIdentity(r.Context(), identityOf(u)), u.Username, "Create administrator account", "")
		a.startSession(w, r, &u)
		sendJSON(w, 201, identityOf(u))
		return nil
	}))
	mux.Handle("POST /api/session", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		if r.Header.Get("Authorization") != "" {
			if !a.bearer(r) {
				return fail(401, "The admin token was not accepted.")
			}
			a.startSession(w, r, nil)
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		u, err := a.accounts.login(body.Username, body.Password)
		if err != nil {
			return err
		}
		a.startSession(w, r, &u)
		sendJSON(w, 200, identityOf(u))
		return nil
	}))
	mux.Handle("GET /api/invites/{secret}", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		u, err := a.accounts.inviteInfo(r.PathValue("secret"))
		if err == nil {
			sendJSON(w, 200, map[string]any{"username": u.Username, "name": u.Name, "role": u.Role, "reset": u.Invite.Reset})
		}
		return err
	}))
	mux.Handle("POST /api/invites/{secret}", endpoint(func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		u, err := a.accounts.redeem(r.PathValue("secret"), body.Password)
		if err != nil {
			return err
		}
		m.noteUser(withIdentity(r.Context(), identityOf(u)), u.Username, "Set account password", "")
		a.startSession(w, r, &u)
		sendJSON(w, 200, identityOf(u))
		return nil
	}))

	add("GET /api/session", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		id, _ := identityFrom(r.Context())
		sendJSON(w, 200, id)
		return nil
	})
	add("DELETE /api/session", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		a.endSession(w, r)
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	add("GET /api/users", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		sendJSON(w, 200, a.accounts.views())
		return nil
	})
	add("POST /api/users", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var body struct {
			Username string `json:"username"`
			Name     string `json:"name"`
			Role     string `json:"role"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		u, secret, err := a.accounts.create(body.Username, body.Name, body.Role)
		if err != nil {
			return err
		}
		m.noteUser(r.Context(), u.Username, "Invite user", "Role: "+u.Role)
		sendJSON(w, 201, map[string]any{"user": u.view(), "secret": secret})
		return nil
	})
	add("PATCH /api/users/{id}", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		var c accountChange
		if err := decode(w, r, &c); err != nil {
			return err
		}
		actor, _ := identityFrom(r.Context())
		before, after, err := a.accounts.update(actor, r.PathValue("id"), c)
		if err != nil {
			return err
		}
		if detail := describeChange(before, after); detail != "" {
			m.noteUser(r.Context(), after.Username, "Update user", detail)
		}
		sendJSON(w, 200, after.view())
		return nil
	})
	add("POST /api/users/{id}/reset", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		actor, _ := identityFrom(r.Context())
		u, secret, err := a.accounts.reset(actor, r.PathValue("id"))
		if err != nil {
			return err
		}
		m.noteUser(r.Context(), u.Username, "Reset password", "Signed out; a new link was issued")
		sendJSON(w, 200, map[string]any{"user": u.view(), "secret": secret})
		return nil
	})
	add("DELETE /api/users/{id}", roleAdmin, func(w http.ResponseWriter, r *http.Request) error {
		actor, _ := identityFrom(r.Context())
		u, err := a.accounts.remove(actor, r.PathValue("id"))
		if err != nil {
			return err
		}
		m.noteUser(r.Context(), u.Username, "Delete user", "")
		w.WriteHeader(http.StatusNoContent)
		return nil
	})

	add("PATCH /api/account", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		id, err := accountOf(r)
		if err != nil {
			return err
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		u, err := a.accounts.rename(id.ID, body.Name)
		if err == nil {
			sendJSON(w, 200, identityOf(u))
		}
		return err
	})
	add("POST /api/account/password", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		id, err := accountOf(r)
		if err != nil {
			return err
		}
		var body struct {
			Current  string `json:"current"`
			Password string `json:"password"`
		}
		if err := decode(w, r, &body); err != nil {
			return err
		}
		u, err := a.accounts.changePassword(id.ID, body.Current, body.Password)
		if err != nil {
			return err
		}
		m.noteUser(r.Context(), u.Username, "Change password", "Other sessions were signed out")
		a.startSession(w, r, &u) // keep this browser signed in
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	add("POST /api/account/signout", roleViewer, func(w http.ResponseWriter, r *http.Request) error {
		id, err := accountOf(r)
		if err != nil {
			return err
		}
		u, err := a.accounts.bump(id.ID, "")
		if err != nil {
			return err
		}
		m.noteUser(r.Context(), u.Username, "Sign out other sessions", "")
		a.startSession(w, r, &u)
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}
