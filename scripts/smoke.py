"""Exercise the installed control plane and both real database engines."""

import datetime
import http.cookiejar
import json
import os
import secrets
import socket
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
        exercise(database_engine, quota == "supported")
