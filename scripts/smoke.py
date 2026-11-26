"""Exercise the installed control plane and both real database engines."""

import datetime
import http.cookiejar
import json
import os
import secrets
import socket
import ssl
import subprocess
import time
import urllib.error
import urllib.request


def cli(*args, env=None):
    return subprocess.check_output(args, env=env, text=True).strip()


address = cli("docker", "compose", "port", "openrds", "8080")
token = cli("docker", "compose", "exec", "-T", "openrds", "openrds", "api-key")


def api(path, method="GET", body=None):
    req = urllib.request.Request(
        f"http://{address}/api/{path}", method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=1800) as response:
            payload = response.read()
            if path.endswith("/download"):
                return payload
            return json.loads(payload) if payload else None
    except urllib.error.HTTPError as err:
        raise RuntimeError(f"{method} {path}: {err.read().decode()}") from err


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def sql(engine, name, query):
    c = api(f"instances/{name}/connection")
    # Use a sibling container on the database's network: this works with
    # native Linux Docker and Desktop, without installing host clients.
    if engine == "postgres":
        variable = "PGPASSWORD"
        cmd = ["postgres:17-alpine", "psql", "-XAt", "-v", "ON_ERROR_STOP=1",
               "-h", c["networkHost"], "-U", c["user"], "-d", c["database"], "-c", query]
    elif engine == "mysql":
        variable = "MYSQL_PWD"
        cmd = ["mysql:8.4", "mysql", "--batch", "--skip-column-names",
               "-h", c["networkHost"], "-u", c["user"], "-D", c["database"], "-e", query]
    else:
        variable = "REDISCLI_AUTH"
        cmd = ["valkey/valkey:9.1-alpine", "valkey-cli", "-h", c["networkHost"], "--no-auth-warning", *query]
    return cli("docker", "run", "--rm", "--network", c["network"], "--env", variable, *cmd,
               env={**os.environ, variable: c["password"]})


PARAMETERS = {"postgres": ("max_connections", "150"), "mysql": ("max_connections", "300"), "valkey": ("maxmemory-policy", "allkeys-lru")}


def setting(engine, name, key):
    if engine == "postgres":
        return sql(engine, name, f"SHOW {key}")
    if engine == "mysql":
        return sql(engine, name, f"SELECT @@{key}")
    return sql(engine, name, ["CONFIG", "GET", key]).splitlines()[-1]


def distant_window():
    """A backup window 12 hours away, so the scheduler never competes with the test."""
    start = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=12)
    return f"{start:%H:%M}-{start + datetime.timedelta(minutes=30):%H:%M}"


def refused(path, method, status):
    try:
        api(path, method)
    except RuntimeError as err:
        assert isinstance(err.__cause__, urllib.error.HTTPError) and err.__cause__.code == status, err
    else:
        raise AssertionError(f"{method} {path} was not refused")


def container_of(network):
    return cli("docker", "ps", "--filter", f"network={network}",
               "--filter", "label=io.openrds.database=true", "--format", "{{.ID}}")


def write_value(engine, name):
    if engine == "valkey":
        assert sql(engine, name, ["SET", "smoke", "42"]) == "OK"
    else:
        sql(engine, name, "CREATE TABLE smoke (id INTEGER PRIMARY KEY); INSERT INTO smoke VALUES (42)")


def read_value(engine, name):
    if engine == "valkey":
        return sql(engine, name, ["GET", "smoke"])
    return sql(engine, name, "SELECT id FROM smoke")


def exercise(engine, quotas):
    name = "smoke-" + secrets.token_hex(5)
    target = name + "-restore"
    backup = None
    try:
        port = free_port()
        memory = {"postgres": 256, "mysql": 1024, "valkey": 128}[engine]
        if not quotas:
            try:
                api("instances", "POST", {"name": name, "engine": engine, "port": port, "diskGiB": 1})
            except RuntimeError as err:
                assert isinstance(err.__cause__, urllib.error.HTTPError) and err.__cause__.code == 422
            else:
                raise AssertionError("unsupported quota accepted")
        key, value = PARAMETERS[engine]
        api("instances", "POST", {"name": name, "engine": engine, "port": port, "cpus": 1, "memoryMiB": memory,
                                  "diskGiB": int(quotas), "backupRetentionDays": 1, "backupWindow": distant_window(), "deletionProtection": True,
                                  "parameters": {key: value}, "tags": {"suite": "smoke"}})
        network = api(f"instances/{name}/connection")["network"]
        container = container_of(network)
        limits = cli("docker", "inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", container)
        assert limits == f"1000000000 {memory * 1024 * 1024}"
        internal_port = {"postgres": "5432/tcp", "mysql": "3306/tcp", "valkey": "6379/tcp"}[engine]
        assert cli("docker", "port", container, internal_port) == f"127.0.0.1:{port}"
        with socket.create_connection(("127.0.0.1", port), timeout=5):
            pass
        write_value(engine, name)
        assert setting(engine, name, key) == value
        assert api(f"instances/{name}/metrics")["CPUPerc"]
        assert api(f"instances/{name}/logs")["text"]
        assert api(f"instances/{name}/storage")["usedBytes"] > 0
        # A deferred change waits for a reboot; the reboot applies it and keeps the data.
        change = api(f"instances/{name}/modify", "POST", {"memoryMiB": memory * 2})
        assert change == {"restarted": False, "pendingRestart": True}, change
        assert cli("docker", "inspect", "--format", "{{.HostConfig.Memory}}", container) == str(memory * 1024 * 1024)
        api(f"instances/{name}/reboot", "POST")
        db = next(d for d in api("instances") if d["name"] == name)
        assert db["status"] == "running" and "applied" not in db and db["nextBackupAt"], db
        container = container_of(network)
        assert cli("docker", "inspect", "--format", "{{.HostConfig.Memory}}", container) == str(memory * 2 * 1024 * 1024)
        assert read_value(engine, name) == "42" and setting(engine, name, key) == value
        for _ in range(30):
            history = api(f"instances/{name}/history?range=1h")
            if history["points"]:
                break
            time.sleep(3)
        assert history["available"] and history["points"][-1]["memory"] > 0, history
        backup = api(f"instances/{name}/backups", "POST")
        assert backup.get("diskGiB", 0) == int(quotas)
        assert backup["bytes"] == len(api(f"backups/{backup['id']}/download")) > 0
        api(f"instances/{name}/stop", "POST")
        assert next(d for d in api("instances") if d["name"] == name)["status"] == "stopped"
        if quotas:
            mounts = json.loads(cli("docker", "inspect", "--format", "{{json .Mounts}}", container))
            volume = next(m["Name"] for m in mounts if m["Type"] == "volume")
            opts = json.loads(cli("docker", "volume", "inspect", volume))[0]
            assert opts["Driver"] == "local" and opts["Options"]["type"] == "ext4"
            assert opts["Options"]["device"].startswith("/dev/openrds/openrds-db-")
            # The capped filesystem must reject an allocation that still fits on
            # an ordinary volume, so host exhaustion cannot pass this check.
            cli("docker", "run", "--rm", "--network", "none", "--mount",
                f"type=volume,src={volume},dst=/data,volume-nocopy", "--mount",
                "type=volume,dst=/reference,volume-nocopy", "--entrypoint", "sh",
                "docker:29-cli", "-ec", '''
free=$(( $(stat -f -c %a /reference) * $(stat -f -c %S /reference) ))
[ "$free" -gt 1073745920 ] || { echo "Need >1 GiB free outside the capped volume" >&2; exit 1; }
f=$(mktemp /data/.openrds-quota-test.XXXXXX)
trap 'rm -f "$f"' EXIT
fallocate -l 4096 "$f"
if message=$(LC_ALL=C fallocate -l 1073745920 "$f" 2>&1); then
    echo "Allocation above 1 GiB unexpectedly succeeded" >&2; exit 1
fi
case "$message" in *"Disk quota exceeded"*|*"No space left on device"*) ;; *) echo "$message" >&2; exit 1 ;; esac
''')
            def filesystem_kb():
                out = cli("docker", "run", "--rm", "--network", "none", "--mount",
                          f"type=volume,src={volume},dst=/data,volume-nocopy", "--entrypoint", "df",
                          "docker:29-cli", "-Pk", "/data")
                return int(out.splitlines()[-1].split()[1])
            before = filesystem_kb()
            api(f"instances/{name}/disk", "POST", {"diskGiB": 2})
            assert next(d for d in api("instances") if d["name"] == name)["diskGiB"] == 2
            assert filesystem_kb() > before + 500_000
            api(f"instances/{name}/disk", "POST", {"diskGiB": 1})
            assert next(d for d in api("instances") if d["name"] == name)["diskGiB"] == 1
            assert filesystem_kb() < before + 200_000
        api(f"instances/{name}/start", "POST")
        assert read_value(engine, name) == "42"
        refused(f"instances/{name}", "DELETE", 409)  # deletion protection
        api(f"instances/{name}/modify", "POST", {"deletionProtection": False})
        api(f"instances/{name}?finalSnapshot=true", "DELETE")
        final = next(b for b in api("backups") if b["name"] == name and b.get("final"))
        assert final["kind"] == "manual" and final["parameters"] == {key: value}
        api(f"backups/{final['id']}/restore", "POST", {"name": target, "port": port})
        assert next(d for d in api("instances") if d["name"] == target)["diskGiB"] == int(quotas)
        assert read_value(engine, target) == "42"
        assert setting(engine, target, key) == value  # parameters travel with the snapshot
        mode = "1 GiB limit enforced and resized" if quotas else "unsupported caps refused; uncapped volume"
        print(f"{engine}: provision, parameters, deferred modify, reboot, history, backup, protection, "
              f"final snapshot and restore passed ({mode})", flush=True)
    finally:
        # Clean up only resources created by this run.
        for db in api("instances"):
            if db["name"] in (target, name):
                if db["deletionProtection"]:
                    api(f"instances/{db['name']}/modify", "POST", {"deletionProtection": False})
                api(f"instances/{db['name']}", "DELETE")
        for b in api("backups"):
            if b["name"] == name:
                api(f"backups/{b['id']}", "DELETE")


def server_version(engine, name):
    """The major version the running server reports, in the catalog's form."""
    if engine == "postgres":
        return str(int(sql(engine, name, "SHOW server_version_num")) // 10000)
    if engine == "mysql":
        return ".".join(sql(engine, name, "SELECT VERSION()").split(".")[:2])
    info = dict(line.strip().split(":", 1) for line in sql(engine, name, ["INFO", "server"]).splitlines() if ":" in line)
    return ".".join(info.get("valkey_version", info.get("redis_version", "")).split(".")[:2])


def exercise_versions(quotas):
    """The oldest listed version upgrades to the latest by restore, and never back."""
    catalog = {e["key"]: e for e in api("engines")}
    for engine in ("postgres", "mysql", "valkey"):
        versions = [v["version"] for v in catalog[engine]["versions"]]
        latest, oldest = versions[0], versions[-1]
        assert catalog[engine]["defaultVersion"] in versions, catalog[engine]
        name = "ver-" + secrets.token_hex(4)
        target, older = name + "-up", name + "-down"
        try:
            port = free_port()
            try:
                api("instances", "POST", {"name": name, "engine": engine, "version": "1.0", "port": port})
            except RuntimeError as err:
                assert isinstance(err.__cause__, urllib.error.HTTPError) and err.__cause__.code == 400, err
            else:
                raise AssertionError("unlisted version accepted")
            assert all(d["name"] != name for d in api("instances"))
            api("instances", "POST", {"name": name, "engine": engine, "version": oldest, "port": port})
            db = next(d for d in api("instances") if d["name"] == name)
            assert db["version"] == oldest and db["engineLabel"].endswith(" " + oldest), db
            assert server_version(engine, name) == oldest
            write_value(engine, name)
            snap = api(f"instances/{name}/backups", "POST")
            assert snap["version"] == oldest, snap
            # A capped volume, when supported, also covers PostgreSQL 18+'s data path.
            api(f"backups/{snap['id']}/restore", "POST", {"name": target, "port": free_port(), "version": latest, "diskGiB": int(quotas)})
            assert next(d for d in api("instances") if d["name"] == target)["version"] == latest
            assert server_version(engine, target) == latest and read_value(engine, target) == "42"
            newer = api(f"instances/{target}/backups", "POST")
            assert newer["version"] == latest, newer
            if latest != oldest:
                try:
                    api(f"backups/{newer['id']}/restore", "POST", {"name": older, "port": free_port(), "version": oldest})
                except RuntimeError as err:
                    assert isinstance(err.__cause__, urllib.error.HTTPError) and err.__cause__.code == 400, err
                else:
                    raise AssertionError("restore into an older version accepted")
            # In place: the same database moves to the latest version and keeps its port, credentials and data.
            before = api(f"instances/{name}/connection")
            api(f"instances/{name}/upgrade", "POST", {"version": latest})
            after = api(f"instances/{name}/connection")
            assert (after["port"], after["password"], after["network"]) == (before["port"], before["password"], before["network"]), after
            assert server_version(engine, name) == latest and read_value(engine, name) == "42"
            assert any(b["name"] == name and b["version"] == oldest and "Before the upgrade" in b.get("note", "") for b in api("backups"))
            try:
                api(f"instances/{name}/upgrade", "POST", {"version": oldest})
            except RuntimeError as err:
                assert isinstance(err.__cause__, urllib.error.HTTPError) and err.__cause__.code == 400, err
            else:
                raise AssertionError("an upgrade to an older version was accepted")
            print(f"{engine}: {oldest} snapshot restored into {latest}, upgraded in place keeping the endpoint; unlisted and older versions refused", flush=True)
        finally:
            for db in api("instances"):
                if db["name"] in (name, target, older):
                    api(f"instances/{db['name']}", "DELETE")
            for b in api("backups"):
                if b["name"] in (name, target):
                    api(f"backups/{b['id']}", "DELETE")


_ca = None


def ca_pem():
    global _ca
    if _ca is None:
        req = urllib.request.Request(f"http://{address}/api/tls/ca.pem", headers={"Authorization": f"Bearer {token}"})
        with urllib.request.urlopen(req, timeout=60) as response:
            _ca = response.read().decode()
    return _ca


def client(engine, c, query, tls=None):
    """Run a real client next to the database. tls: None (engine default), "off" or "verify"."""
    host = c["networkHost"]
    if engine == "postgres":
        image, variable = "postgres:17-alpine", "PGPASSWORD"
        mode = {None: "sslmode=prefer", "off": "sslmode=disable", "verify": "sslmode=verify-full sslrootcert=/tmp/ca.pem"}[tls]
        run = f'psql -XAt -v ON_ERROR_STOP=1 "host={host} port={c.get("networkPort", 5432)} user={c["user"]} dbname={c["database"]} {mode}" -c "$Q"'
    elif engine == "mysql":
        image, variable = "mysql:8.4", "MYSQL_PWD"
        mode = {None: "", "off": "--ssl-mode=DISABLED", "verify": "--ssl-mode=VERIFY_IDENTITY --ssl-ca=/tmp/ca.pem"}[tls]
        run = f'mysql --batch --skip-column-names {mode} -h {host} -u {c["user"]} -D {c["database"]} -e "$Q"'
    else:
        image, variable = "valkey/valkey:9.1-alpine", "REDISCLI_AUTH"
        mode = "--tls --cacert /tmp/ca.pem" if tls == "verify" else ""
        run = f'valkey-cli -h {host} --user {c["user"]} --no-auth-warning {mode} $Q'
        query = " ".join(query)
    script = 'printf "%s" "$CA" > /tmp/ca.pem; ' + run
    env = {**os.environ, variable: c["password"], "CA": ca_pem(), "Q": query}
    done = subprocess.run(["docker", "run", "--rm", "--network", c["network"], "--env", variable, "--env", "CA", "--env", "Q",
                           "--entrypoint", "sh", image, "-c", script], env=env, capture_output=True, text=True)
    out = (done.stdout + done.stderr).strip()
    ok = done.returncode == 0 and not any(code in out for code in ("NOPERM", "NOAUTH", "WRONGPASS", "ERR "))
    return ok, out


def must(engine, c, query, tls=None):
    ok, out = client(engine, c, query, tls)
    assert ok, f"{engine} {c['user']}: {query!r} failed: {out}"
    return out.splitlines()[-1] if out else ""


def refuses(engine, c, query, tls=None):
    ok, out = client(engine, c, query, tls)
    assert not ok, f"{engine} {c['user']}: {query!r} should have been refused: {out}"


def reachable(engine, port):
    """Whether the public port answers like the database, through the gateway."""
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=5) as s:
            if engine == "postgres":
                s.sendall(b"\x00\x00\x00\x08\x04\xd2\x16\x2f")  # SSLRequest
                return s.recv(1) == b"S"
            if engine == "valkey":  # TLS only by now
                ctx = ssl.create_default_context()
                ctx.check_hostname, ctx.verify_mode = False, ssl.CERT_NONE
                with ctx.wrap_socket(s) as t:
                    t.sendall(b"PING\r\n")
                    return t.recv(64).startswith(b"-NOAUTH")
            return len(s.recv(64)) > 0  # the MySQL greeting
    except (OSError, ssl.SSLError):
        return False


def pgbench(c, args):
    """Run pgbench next to the database, as c's user."""
    done = subprocess.run(["docker", "run", "--rm", "--network", c["network"], "--env", "PGPASSWORD", "--entrypoint", "pgbench",
                           "postgres:17-alpine", "-h", c["networkHost"], "-p", str(c.get("networkPort", 5432)), "-U", c["user"], *args, c["database"]],
                          env={**os.environ, "PGPASSWORD": c["password"]}, capture_output=True, text=True)
    return done.returncode, done.stdout + done.stderr


def exercise_pooler():
    """PgBouncer in front of PostgreSQL: more clients than max_connections, users, passwords and TLS."""
    name = f"smoke-pool-{secrets.token_hex(3)}"
    api("instances", "POST", {"name": name, "engine": "postgres", "port": free_port(), "parameters": {"max_connections": "20"}})
    try:
        result = api(f"instances/{name}/modify", "POST", {"pooler": {"enabled": True, "port": free_port()}})
        assert result == {"restarted": False, "pendingRestart": False}, result
        d = next(i for i in api("instances") if i["name"] == name)
        assert d["pooler"]["mode"] == "transaction" and d["poolerStatus"] == "running", d

        def pooled(c):
            return {**c, "networkHost": f"openrds-{name}-pooler", "networkPort": 6432}
        owner = api(f"instances/{name}/connection")
        assert must("postgres", pooled(owner), "SELECT 1") == "1"
        code, out = pgbench(owner, ["-i", "-q"])
        assert code == 0, out
        code, out = pgbench(owner, ["-c", "50", "-j", "4", "-T", "5", "-S"])
        assert code != 0 and "too many clients" in out, "50 direct clients should exceed max_connections 20"
        code, out = pgbench(pooled(owner), ["-c", "50", "-j", "4", "-T", "5", "-S", "-M", "prepared"])
        assert code == 0 and "number of failed transactions: 0" in out, out

        api(f"instances/{name}/users", "POST", {"name": "app", "access": "read-write"})
        assert must("postgres", pooled(api(f"instances/{name}/connection?user=app")), "SELECT current_user") == "app"
        api(f"instances/{name}/password", "POST", {"password": ""})
        fresh = api(f"instances/{name}/connection")
        must("postgres", pooled(fresh), "SELECT 1")
        refuses("postgres", pooled(owner), "SELECT 1")
        # Two changes within a second still reach the pooler.
        api(f"instances/{name}/password", "POST", {"password": ""})
        api(f"instances/{name}/password", "POST", {"password": ""})
        fresh = api(f"instances/{name}/connection")
        must("postgres", pooled(fresh), "SELECT 1")

        assert api(f"instances/{name}/modify", "POST", {"requireTLS": True, "applyImmediately": True})["restarted"]
        must("postgres", pooled(fresh), "SELECT 1", tls="verify")
        refuses("postgres", pooled(fresh), "SELECT 1", tls="off")

        assert api(f"instances/{name}/modify", "POST", {"pooler": {"enabled": False}}) == {"restarted": False, "pendingRestart": False}
        assert "pooler" not in next(i for i in api("instances") if i["name"] == name)
        refuses("postgres", pooled(fresh), "SELECT 1")
        print("postgres: connection pooling with more clients than max_connections, users, passwords and TLS passed", flush=True)
    finally:
        api(f"instances/{name}", "DELETE")


def exercise_security(engine):
    name = "sec-" + secrets.token_hex(5)
    try:
        api("instances", "POST", {"name": name, "engine": engine, "port": free_port(), "memoryMiB": {"postgres": 256, "mysql": 1024, "valkey": 128}[engine]})
        db = next(d for d in api("instances") if d["name"] == name)
        assert db["tlsReady"] and db["users"] == [] and not db["publicAccess"]
        owner = api(f"instances/{name}/connection")
        read = ["GET", "smoke"] if engine == "valkey" else "SELECT id FROM smoke WHERE id = 42"
        write = ["SET", "written", "1"] if engine == "valkey" else "INSERT INTO smoke VALUES (7)"
        write_again = ["SET", "written", "2"] if engine == "valkey" else "INSERT INTO smoke VALUES (8)"
        write_value(engine, name)

        # Extra users: read-only can read but not write; read-write can write.
        api(f"instances/{name}/users", "POST", {"name": "reader", "access": "read-only"})
        api(f"instances/{name}/users", "POST", {"name": "writer", "access": "read-write"})
        reader = api(f"instances/{name}/connection?user=reader")
        writer = api(f"instances/{name}/connection?user=writer")
        assert must(engine, reader, read) == "42"
        refuses(engine, reader, write)
        must(engine, writer, write)
        assert [u["name"] for u in next(d for d in api("instances") if d["name"] == name)["users"]] == ["reader", "writer"]

        # Changing the owner password retires the old one at once.
        new = api(f"instances/{name}/password", "POST", {})["password"]
        assert new != owner["password"]
        refuses(engine, owner, read)
        owner = api(f"instances/{name}/connection")
        assert owner["password"] == new and must(engine, owner, read) == "42"

        # Requiring TLS: plain text is refused, verified TLS works, and users
        # (for Valkey, kept in its users file) survive the restart.
        api(f"instances/{name}/modify", "POST", {"requireTLS": True, "applyImmediately": True})
        owner, writer, reader = (api(f"instances/{name}/connection{q}") for q in ("", "?user=writer", "?user=reader"))
        if engine != "valkey":
            refuses(engine, owner, read, tls="off")
        else:
            refuses(engine, owner, read)  # plain text on a TLS-only port
        assert must(engine, owner, read, tls="verify") == "42"
        must(engine, writer, write_again, tls="verify")
        refuses(engine, reader, write, tls="verify")
        backup = api(f"instances/{name}/backups", "POST")
        assert backup["bytes"] > 0
        api(f"backups/{backup['id']}", "DELETE")

        # Deleting a user revokes it.
        api(f"instances/{name}/users/reader", "DELETE")
        refuses(engine, reader, read, tls="verify")

        # Public access: the gateway forwards only allowed sources.
        public = free_port()
        api(f"instances/{name}/modify", "POST", {"publicAccess": True, "publicPort": public,
                                                 "firewall": [{"cidr": "203.0.113.0/24", "description": "documentation range"}]})
        assert not reachable(engine, public)
        api(f"instances/{name}/modify", "POST", {"firewall": [{"cidr": "0.0.0.0/0"}]})
        deadline = time.time() + 20
        while not reachable(engine, public):
            assert time.time() < deadline, "the allowed source never got through"
            time.sleep(1)
        attempts = api(f"instances/{name}/connections")
        assert attempts and attempts[0]["allowed"], attempts
        api(f"instances/{name}/modify", "POST", {"publicAccess": False})
        assert not reachable(engine, public)
        events = [e["action"] for e in api("activity") if e["name"] == name]
        assert {"Create database user", "Change owner password", "Reveal credentials", "Change network access"} <= set(events), events
        print(f"{engine}: users, password change, required TLS, revocation and firewall passed", flush=True)
    finally:
        for db in api("instances"):
            if db["name"] == name:
                api(f"instances/{name}", "DELETE")


def wait_for(what, ok, seconds=90):
    deadline = time.time() + seconds
    while True:
        result = ok()
        if result:
            return result
        assert time.time() < deadline, f"timed out waiting for {what}"
        time.sleep(2)


def exercise_storage():
    """Off-server copies in an S3-compatible gateway that checks signatures."""
    suffix = secrets.token_hex(4)
    name, restored, gateway, bucket = f"s3-{suffix}", f"s3-{suffix}-restore", f"smoke-s3-{suffix}", "openrds-smoke"
    pitr, rewound = f"pitr-{suffix}", f"pitr-{suffix}-at"
    control = cli("docker", "compose", "ps", "-q", "openrds")
    network = cli("docker", "inspect", "-f", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", control).split()[0]
    secret = secrets.token_hex(16)
    cli("docker", "run", "-d", "--name", gateway, "--network", network, "-e", "ROOT_ACCESS_KEY=smoke-access",
        "-e", "ROOT_SECRET_KEY", "versity/versitygw:latest", "posix", "/tmp", env={**os.environ, "ROOT_SECRET_KEY": secret})
    settings = {"provider": "other", "endpoint": f"http://{gateway}:7070", "region": "us-east-1", "bucket": bucket,
                "prefix": "smoke/", "accessKeyId": "smoke-access", "secretAccessKey": secret, "pathStyle": True}
    try:
        wait_for("the gateway", lambda: subprocess.run(["docker", "exec", gateway, "mkdir", "-p", f"/tmp/{bucket}"]).returncode == 0, 30)
        wrong = {**settings, "secretAccessKey": "not-the-secret-0000"}
        try:
            api("storage/test", "POST", wrong)
        except RuntimeError as err:
            assert "access denied" in str(err), err
        else:
            raise AssertionError("a wrong secret was accepted")
        wait_for("the connection", lambda: api("storage", "PUT", settings)["enabled"], 30)
        api("instances", "POST", {"name": name, "engine": "postgres", "port": free_port(), "memoryMiB": 256})
        write_value("postgres", name)
        backup = api(f"instances/{name}/backups", "POST")
        wait_for("the copy", lambda: next(b for b in api("backups") if b["id"] == backup["id"]).get("offsite", {}).get("status") == "copied")
        remote = {b["id"]: b for b in api("storage/backups")}
        assert remote[backup["id"]]["local"], remote
        status = api("storage")
        assert status["copied"] >= 1 and "secretAccessKey" not in json.dumps(status)

        # Lose the server's copy: disconnect, delete the backup, reconnect, import and restore.
        api("storage", "DELETE")
        api(f"backups/{backup['id']}", "DELETE")
        api("storage", "PUT", settings)
        remote = {b["id"]: b for b in api("storage/backups")}
        assert not remote[backup["id"]]["local"], remote
        imported = api(f"storage/backups/{backup['id']}/import", "POST")
        assert imported["kind"] == "manual" and imported["bytes"] == backup["bytes"]
        api(f"backups/{backup['id']}/restore", "POST", {"name": restored, "port": free_port()})
        assert read_value("postgres", restored) == "42"

        # Deleting from the library deletes the copy too.
        api(f"backups/{backup['id']}", "DELETE")
        wait_for("the copy to be deleted", lambda: backup["id"] not in {b["id"] for b in api("storage/backups")}, 60)

        # Point-in-time recovery after losing the server: the base backup and the WAL come back from storage.
        api("instances", "POST", {"name": pitr, "engine": "postgres", "port": free_port(), "memoryMiB": 256, "backupRetentionDays": 1, "pitr": True})
        base = wait_for("the first base backup", lambda: next((b for b in api("backups") if b["name"] == pitr and b.get("format") == "physical"), None), 180)
        write_value("postgres", pitr)
        time.sleep(2)
        target = datetime.datetime.now(datetime.timezone.utc)
        time.sleep(2)
        sql("postgres", pitr, "UPDATE smoke SET id = 43")
        sql("postgres", pitr, "SELECT pg_switch_wal()")
        key = base["dataKey"]
        local = lambda: set(cli("docker", "compose", "exec", "-T", "openrds", "sh", "-c", f"ls /var/lib/openrds/pitr/{key} | grep gz$ || true").split())
        remote = lambda: set(cli("docker", "exec", gateway, "sh", "-c", f"ls /tmp/{bucket}/smoke/wal/{key} 2>/dev/null || true").split())
        wait_for("WAL past the target", lambda: next(i for i in api("instances") if i["name"] == pitr)["recovery"].get("latest", "") > target.isoformat(), 180)
        wait_for("the WAL copies", lambda: local() <= remote(), 180)
        wait_for("the base backup copy", lambda: next(b for b in api("backups") if b["id"] == base["id"]).get("offsite", {}).get("status") == "copied")
        api("storage", "DELETE")
        api(f"backups/{base['id']}", "DELETE")
        cli("docker", "compose", "exec", "-T", "openrds", "rm", "-rf", f"/var/lib/openrds/pitr/{key}")
        api("storage", "PUT", settings)
        api(f"storage/backups/{base['id']}/import", "POST")
        api(f"backups/{base['id']}/restore", "POST", {"name": rewound, "port": free_port(), "targetTime": target.isoformat()})
        assert read_value("postgres", rewound) == "42", "the value as it was at the target, before the update"
        print("storage: signed copies, import after loss, restore, mirrored deletes and point-in-time recovery from storage passed", flush=True)
    finally:
        try:
            api("storage", "DELETE")
        except RuntimeError:
            pass
        for db in api("instances"):
            if db["name"] in (name, restored, pitr, rewound):
                api(f"instances/{db['name']}", "DELETE")
        for b in api("backups"):
            if b["name"] in (name, restored, pitr, rewound):
                api(f"backups/{b['id']}", "DELETE")
        subprocess.run(["docker", "rm", "-f", gateway], capture_output=True)


def browser():
    """A cookie-keeping client, like one person's browser session."""
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def call(path, method="GET", body=None):
        req = urllib.request.Request(f"http://{address}/api/{path}", method=method,
                                     data=None if body is None else json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"})
        try:
            with opener.open(req, timeout=60) as response:
                payload = response.read()
                return response.status, json.loads(payload) if payload else None
        except urllib.error.HTTPError as err:
            return err.code, None
    return call


def exercise_accounts():
    suffix = secrets.token_hex(3)
    created = []
    try:
        people = {}
        for role in ("operator", "viewer"):
            invite = api("users", "POST", {"username": f"smoke-{role}-{suffix}", "name": f"Smoke {role}", "role": role})
            created.append(invite["user"]["id"])
            assert invite["user"]["status"] == "invited" and "password" not in json.dumps(invite["user"])
            person = browser()
            code, me = person(f"invites/{invite['secret']}", "POST", {"password": f"{role}-{suffix}-password"})
            assert code == 200 and me["role"] == role, (code, me)
            assert person(f"invites/{invite['secret']}", "POST", {"password": "a-second-password"})[0] == 404
            people[role] = person
        viewer, operator = people["viewer"], people["operator"]
        assert viewer("instances")[0] == 200
        assert viewer("instances", "POST", {"name": f"denied-{suffix}", "engine": "valkey", "port": free_port()})[0] == 403
        assert viewer("users")[0] == 403 and operator("users")[0] == 403
        assert operator("session")[1]["role"] == "operator"
        returning = browser()
        assert returning("session", "POST", {"username": f"smoke-viewer-{suffix}", "password": "wrong-password"})[0] == 401
        assert returning("session", "POST", {"username": f"smoke-viewer-{suffix}", "password": f"viewer-{suffix}-password"})[0] == 200
        events = api("activity")
        assert any(e["action"] == "Set account password" and e["actor"] == f"smoke-viewer-{suffix}" for e in events)
        print("accounts: invitations, sign-in, roles and audit trail passed", flush=True)
    finally:
        for user_id in created:
            api(f"users/{user_id}", "DELETE")


if __name__ == "__main__":
    exercise_accounts()
    host = api("host")
    assert host["cpus"] > 0 and host["memoryBytes"] > 0
    quota = host["volumeQuota"]["state"]
    assert quota in ("supported", "unsupported"), host["volumeQuota"]
    if os.environ.get("OPENRDS_REQUIRE_QUOTA") == "1":
        assert quota == "supported", host["volumeQuota"]
    for database_engine in ("postgres", "mysql", "valkey"):
        exercise_security(database_engine)
    exercise_pooler()
    for database_engine in ("postgres", "mysql", "valkey"):
        exercise(database_engine, quota == "supported")
    exercise_versions(quota == "supported")
    exercise_storage()
