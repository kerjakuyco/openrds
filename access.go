package main

// Database credentials: the owner's password, extra users with read-only or
// read-write access, and the engine statements that apply them. Statements
// travel on stdin, so passwords never appear in a process list.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const maxDBUsers = 20

var (
	dbPasswordPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,128}$`)
	dbUserPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reservedUsers     = []string{"openrds", "root", "postgres", "default", "public", "admin", "mysql", "valkey"}
	valkeyError       = regexp.MustCompile(`(?m)^(ERR|NOAUTH|WRONGPASS|NOPERM|LOADING|READONLY|BUSY|MISCONF)\b.*$`)
)

// Never serialize dbUser in API responses: it contains the password.
type dbUser struct {
	Name     string    `json:"name"`
	Access   string    `json:"access"` // read-only or read-write
	Password string    `json:"password"`
	Created  time.Time `json:"createdAt"`
}

type dbUserView struct {
	Name    string    `json:"name"`
	Access  string    `json:"access"`
	Created time.Time `json:"createdAt"`
}

func (r record) userViews() []dbUserView {
	out := make([]dbUserView, 0, len(r.Users))
	for _, u := range r.Users {
		out = append(out, dbUserView{u.Name, u.Access, u.Created})
	}
	return out
}

func (r record) findUser(name string) int {
	return slices.IndexFunc(r.Users, func(u dbUser) bool { return u.Name == name })
}

// A chosen password, or a generated one when none is given. The character
// set is safe unescaped in URLs, shells, SQL and Valkey commands.
func choosePassword(custom string) (string, error) {
	if custom == "" {
		return randomHex(24), nil
	}
	if !dbPasswordPattern.MatchString(custom) {
		return "", fail(400, "Use 16–128 characters for the password: letters, digits and . _ ~ -. Leave it empty to generate one.")
	}
	return custom, nil
}

func checkDBUser(r record, name, access string) error {
	if !dbUserPattern.MatchString(name) {
		return fail(400, "Start the username with a letter and use up to 32 lowercase letters, digits and underscores.")
	}
	if slices.Contains(reservedUsers, name) || strings.HasPrefix(name, "pg_") || strings.HasPrefix(name, "mysql") || strings.HasPrefix(name, "openrds") {
		return fail(400, fmt.Sprintf("%q is reserved. Choose another username.", name))
	}
	if access != "read-only" && access != "read-write" {
		return fail(400, "Choose read-only or read-write access.")
	}
	if r.findUser(name) >= 0 {
		return fail(409, fmt.Sprintf("%s already has a user called %s.", r.Name, name))
	}
	if len(r.Users) >= maxDBUsers {
		return fail(409, fmt.Sprintf("A database can have at most %d extra users.", maxDBUsers))
	}
	return nil
}

// A PostgreSQL SCRAM-SHA-256 verifier, so the server never sees the
// password itself, even in statement logs.
func scramVerifier(password string, salt []byte, iterations int) string {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		panic(err)
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac(salted, "Server Key")))
}

func pgPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	return scramVerifier(password, salt, 4096)
}

func valkeyHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

var valkeyRules = map[string]string{
	"read-only":  "-@all +@read +@connection -@dangerous",
	"read-write": "+@all -@admin -@dangerous",
}

func valkeyUserRule(u dbUser) string {
	return fmt.Sprintf("%s on #%s ~* &* %s", u.Name, valkeyHash(u.Password), valkeyRules[u.Access])
}

// The Valkey users file: the owner as the default user, then extra users.
// Passwords are stored as SHA-256 digests, which Valkey checks directly.
func valkeyUsers(r record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "user default on #%s ~* &* +@all\n", valkeyHash(r.Password))
	for _, u := range r.Users {
		fmt.Fprintf(&b, "user %s\n", valkeyUserRule(u))
	}
	switch {
	case r.ReplicaOf != "":
		b.WriteString(valkeyReplicaConfig(r))
	case r.ReplicationPassword != "":
		// Replicas sign in as this user, which can only replicate.
		fmt.Fprintf(&b, "user %s on #%s +psync +replconf +ping\n", replicationUser, valkeyHash(r.ReplicationPassword))
	}
	return b.String()
}

// Run statements inside the container as the engine's local administrator.
func (m *manager) admin(ctx context.Context, r record, script string) error {
	_, err := m.query(ctx, r, script)
	return err
}

// Like admin, returning what the statements print.
func (m *manager) query(ctx context.Context, r record, script string) (string, error) {
	var args []string
	input := script
	switch r.Engine {
	case "postgres":
		args = []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "openrds", "-d", "app"}
	case "mysql":
		args = []string{"sh", "-ec", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql -u root -N -B`}
	case "valkey":
		// The first line authenticates; the rest are commands.
		args = []string{"sh", "-ec", `IFS= read -r REDISCLI_AUTH; export REDISCLI_AUTH; exec valkey-cli --no-auth-warning $OPENRDS_CLI_FLAGS`}
		input = r.Password + "\n" + script
	}
	var out bytes.Buffer
	err := m.compose(ctx, r, strings.NewReader(input), &out, append([]string{"exec", "-T", "db"}, args...)...)
	if err == nil && r.Engine == "valkey" {
		if line := valkeyError.FindString(out.String()); line != "" {
			err = fmt.Errorf("valkey: %s", line)
		}
	}
	return out.String(), err
}

// Replace the users file in a running Valkey container, so a restart by
// Docker keeps the change. Compose writes the same file when it recreates
// the container.
func (m *manager) writeValkeyUsers(ctx context.Context, r record) error {
	script := `f=/run/openrds/users.conf; cat > "$f.new"; chown 999:1000 "$f.new"; chmod 0400 "$f.new"; mv "$f.new" "$f"`
	return m.compose(ctx, r, strings.NewReader(valkeyUsers(r)), nil, "exec", "-T", "db", "sh", "-ec", script)
}

func pgIdent(name string) string { return `"` + name + `"` }

func grantScript(r record, u dbUser) string {
	switch r.Engine {
	case "postgres":
		s := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s';\nGRANT pg_read_all_data TO %[1]s;\n", pgIdent(u.Name), pgPassword(u.Password))
		if u.Access == "read-write" {
			s += fmt.Sprintf("GRANT pg_write_all_data TO %s;\n", pgIdent(u.Name))
		}
		return s
	case "mysql":
		privileges := "SELECT, SHOW VIEW"
		if u.Access == "read-write" {
			privileges += ", INSERT, UPDATE, DELETE"
		}
		return fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s';\nGRANT %s ON app.* TO '%[1]s'@'%%';\n", u.Name, u.Password, privileges)
	default:
		return fmt.Sprintf("ACL SETUSER %s\n", strings.ReplaceAll(valkeyUserRule(u), " on ", " reset on "))
	}
}

func passwordScript(r record, user, password string) string {
	switch r.Engine {
	case "postgres":
		return fmt.Sprintf("ALTER ROLE %s PASSWORD '%s';\n", pgIdent(user), pgPassword(password))
	case "mysql":
		return fmt.Sprintf("ALTER USER '%s'@'%%' IDENTIFIED BY '%s';\n", user, password)
	default:
		return fmt.Sprintf("ACL SETUSER %s resetpass #%s\n", user, valkeyHash(password))
	}
}

// Dropping a user also ends its open sessions, so revoking takes effect now.
func dropScript(r record, user string) string {
	switch r.Engine {
	case "postgres":
		return fmt.Sprintf("ALTER ROLE %[1]s NOLOGIN;\nSELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = '%[2]s';\nDROP OWNED BY %[1]s;\nDROP ROLE %[1]s;\n", pgIdent(user), user)
	case "mysql":
		return fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%';\n", user)
	default:
		return fmt.Sprintf("ACL DELUSER %s\n", user)
	}
}

// MySQL keeps sessions of dropped users open, so end them separately.
func (m *manager) endMySQLSessions(ctx context.Context, r record, user string) error {
	script := fmt.Sprintf(`export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
for id in $(mysql -u root -N -B -e "SELECT id FROM information_schema.PROCESSLIST WHERE USER = '%s'"); do
  mysql -u root -e "KILL $id" || true
done`, user)
	return m.compose(ctx, r, nil, nil, "exec", "-T", "db", "sh", "-ec", script)
}

// Credential changes need a running database whose container was started by
// this version, so health checks and restarts keep working afterwards.
func (m *manager) beginAccess(ctx context.Context, name string) (record, func(), error) {
	r, release, err := m.begin(name, nil, false)
	if err != nil {
		return r, nil, err
	}
	states, err := m.states(ctx)
	if err != nil {
		release()
		return r, nil, fail(503, "Cannot read database status: "+err.Error())
	}
	switch {
	case r.ReplicaOf != "":
		err = fail(409, fmt.Sprintf("%s is a read replica: it has the users and passwords of %s. Change them there.", r.Name, r.ReplicaOf))
	case r.PendingRestore:
		err = fail(409, "This instance has an incomplete restore.")
	case states[r.Project] != "running":
		err = fail(409, fmt.Sprintf("Start %s and wait until it is healthy, then try again.", r.Name))
	case r.Layout < currentLayout:
		err = fail(409, fmt.Sprintf("Reboot %s once to turn this on. It was started by an earlier version of OpenRDS.", r.Name))
	}
	if err != nil {
		release()
		return r, nil, err
	}
	return r, release, nil
}

// Apply a credential change to the engine, authenticated with the
// credentials in force before it, then keep it: in the Valkey users file
// and in the record.
func (m *manager) applyAccess(ctx context.Context, current, next record, script string) error {
	if err := m.admin(ctx, current, script); err != nil {
		return fail(502, "The database refused the change: "+err.Error())
	}
	if next.Engine == "valkey" {
		if err := m.writeValkeyUsers(ctx, next); err != nil {
			return fail(502, "The change is active but could not be saved for restarts: "+err.Error())
		}
	}
	if err := m.save(next); err != nil {
		return err
	}
	if next.Pooler.on() {
		if err := m.reloadPoolerUsers(ctx, next); err != nil {
			return fail(502, "The change is active in the database. The connection pooler gets it when it restarts: "+err.Error())
		}
	}
	return m.propagateAccess(ctx, current, next, script)
}

// Change the owner's password (user empty or the owner) or an extra user's.
func (m *manager) changePassword(ctx context.Context, name, user, custom string) (password string, err error) {
	r, release, err := m.beginAccess(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	if password, err = choosePassword(custom); err != nil {
		return "", err
	}
	next := r
	next.Users = slices.Clone(r.Users)
	owner := engines[r.Engine].user
	action := "Change owner password"
	if user == "" || user == owner {
		user = owner
		next.Password = password
	} else {
		i := r.findUser(user)
		if i < 0 {
			return "", fail(404, fmt.Sprintf("%s has no user called %s.", name, user))
		}
		next.Users[i].Password = password
		action = "Change user password"
	}
	finish := m.trackDetail(ctx, name, action, user)
	defer func() { finish(err) }()
	return password, m.applyAccess(ctx, r, next, passwordScript(r, user, password))
}

func (m *manager) createUser(ctx context.Context, name, user, access, custom string) (password string, err error) {
	r, release, err := m.beginAccess(ctx, name)
	if err != nil {
		return "", err
	}
	defer release()
	if err := checkDBUser(r, user, access); err != nil {
		return "", err
	}
	if password, err = choosePassword(custom); err != nil {
		return "", err
	}
	u := dbUser{Name: user, Access: access, Password: password, Created: time.Now().UTC()}
	next := r
	next.Users = append(slices.Clone(r.Users), u)
	finish := m.trackDetail(ctx, name, "Create database user", fmt.Sprintf("%s (%s)", user, access))
	defer func() { finish(err) }()
	return password, m.applyAccess(ctx, r, next, grantScript(r, u))
}

func (m *manager) deleteUser(ctx context.Context, name, user string) (err error) {
	r, release, err := m.beginAccess(ctx, name)
	if err != nil {
		return err
	}
	defer release()
	i := r.findUser(user)
	if i < 0 {
		return fail(404, fmt.Sprintf("%s has no user called %s.", name, user))
	}
	next := r
	next.Users = slices.Delete(slices.Clone(r.Users), i, i+1)
	finish := m.trackDetail(ctx, name, "Delete database user", user)
	defer func() { finish(err) }()
	if err = m.applyAccess(ctx, r, next, dropScript(r, user)); err != nil {
		return err
	}
	if r.Engine == "mysql" {
		if err := m.endMySQLSessions(ctx, next, user); err != nil {
			return fail(502, "The user was deleted, but its open sessions could not be ended: "+err.Error())
		}
	}
	return nil
}

// Connection details for the owner or an extra user. Every reveal is recorded.
func (m *manager) credentials(ctx context.Context, name, user string) (record, string, string, error) {
	r, err := m.record(name)
	if err != nil {
		return r, "", "", err
	}
	login, password := engines[r.Engine].user, r.Password
	if user != "" && user != login {
		i := r.findUser(user)
		if i < 0 {
			return r, "", "", fail(404, fmt.Sprintf("%s has no user called %s.", name, user))
		}
		login, password = r.Users[i].Name, r.Users[i].Password
	}
	m.logEvent(ctx, event{Name: name, Action: "Reveal credentials", Result: "completed", Detail: login})
	return r, login, password, nil
}
