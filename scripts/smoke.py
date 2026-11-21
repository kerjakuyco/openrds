"""Exercise the installed control plane and both real database engines."""

import json
import os
import secrets
import socket
import subprocess
import urllib.error
import urllib.request


def cli(*args, env=None):
    return subprocess.check_output(args, env=env, text=True).strip()


address = cli("docker", "compose", "port", "openrds", "8080")
token = cli("docker", "compose", "exec", "-T", "openrds", "cat", "/var/lib/openrds/admin-token")


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
        api("instances", "POST", {"name": name, "engine": engine, "port": port,
                                  "cpus": 1, "memoryMiB": memory, "diskGiB": int(quotas)})
        network = api(f"instances/{name}/connection")["network"]
        container = cli("docker", "ps", "--filter", f"network={network}",
                        "--filter", "label=io.openrds.database=true", "--format", "{{.ID}}")
        limits = cli("docker", "inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", container)
        assert limits == f"1000000000 {memory * 1024 * 1024}"
        internal_port = {"postgres": "5432/tcp", "mysql": "3306/tcp", "valkey": "6379/tcp"}[engine]
        assert cli("docker", "port", container, internal_port) == f"127.0.0.1:{port}"
        with socket.create_connection(("127.0.0.1", port), timeout=5):
            pass
        write_value(engine, name)
        assert api(f"instances/{name}/metrics")["CPUPerc"]
        assert api(f"instances/{name}/logs")["text"]
        assert api(f"instances/{name}/storage")["usedBytes"] > 0
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
        api(f"instances/{name}", "DELETE")
        api(f"backups/{backup['id']}/restore", "POST", {"name": target, "port": port})
        assert next(d for d in api("instances") if d["name"] == target)["diskGiB"] == int(quotas)
        assert read_value(engine, target) == "42"
        mode = "1 GiB limit enforced and resized" if quotas else "unsupported caps refused; uncapped volume"
        print(f"{engine}: provision, connect, observe, restart, backup and restore passed ({mode})", flush=True)
    finally:
        # Clean up only resources created by this run.
        for db in api("instances"):
            if db["name"] in (target, name):
                api(f"instances/{db['name']}", "DELETE")
        if backup:
            api(f"backups/{backup['id']}", "DELETE")


if __name__ == "__main__":
    host = api("host")
    assert host["cpus"] > 0 and host["memoryBytes"] > 0
    quota = host["volumeQuota"]["state"]
    assert quota in ("supported", "unsupported"), host["volumeQuota"]
    if os.environ.get("OPENRDS_REQUIRE_QUOTA") == "1":
        assert quota == "supported", host["volumeQuota"]
    for database_engine in ("postgres", "mysql", "valkey"):
        exercise(database_engine, quota == "supported")
