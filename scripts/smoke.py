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
    # native Linux Docker and Desktop, without installing host SQL clients.
    if engine == "postgres":
        variable = "PGPASSWORD"
        cmd = ["postgres:17-alpine", "psql", "-XAt", "-v", "ON_ERROR_STOP=1",
               "-h", c["networkHost"], "-U", c["user"], "-d", c["database"], "-c", query]
    else:
        variable = "MYSQL_PWD"
        cmd = ["mysql:8.4", "mysql", "--batch", "--skip-column-names",
               "-h", c["networkHost"], "-u", c["user"], "-D", c["database"], "-e", query]
    return cli("docker", "run", "--rm", "--network", c["network"], "--env", variable, *cmd,
               env={**os.environ, variable: c["password"]})


def exercise(engine):
    name = "smoke-" + secrets.token_hex(5)
    target = name + "-restore"
    backup = None
    try:
        port = free_port()
        memory = 256 if engine == "postgres" else 1024
        api("instances", "POST", {"name": name, "engine": engine, "port": port,
                                  "cpus": 1, "memoryMiB": memory})
        network = api(f"instances/{name}/connection")["network"]
        container = cli("docker", "ps", "--filter", f"network={network}",
                        "--filter", "label=io.openrds.database=true", "--format", "{{.ID}}")
        limits = cli("docker", "inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", container)
        assert limits == f"1000000000 {memory * 1024 * 1024}"
        internal_port = "5432/tcp" if engine == "postgres" else "3306/tcp"
        assert cli("docker", "port", container, internal_port) == f"127.0.0.1:{port}"
        with socket.create_connection(("127.0.0.1", port), timeout=5):
            pass
        sql(engine, name, "CREATE TABLE smoke (id INTEGER PRIMARY KEY); INSERT INTO smoke VALUES (42)")
        assert api(f"instances/{name}/metrics")["CPUPerc"]
        assert api(f"instances/{name}/logs")["text"]
        backup = api(f"instances/{name}/backups", "POST")
        assert backup["bytes"] == len(api(f"backups/{backup['id']}/download")) > 0
        api(f"instances/{name}/stop", "POST")
        assert next(d for d in api("instances") if d["name"] == name)["status"] == "stopped"
        api(f"instances/{name}/start", "POST")
        assert sql(engine, name, "SELECT id FROM smoke") == "42"
        api(f"instances/{name}", "DELETE")
        api(f"backups/{backup['id']}/restore", "POST", {"name": target, "port": port})
        assert sql(engine, target, "SELECT id FROM smoke") == "42"
        print(f"{engine}: provision, connect, observe, restart, backup and restore passed", flush=True)
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
    for database_engine in ("postgres", "mysql"):
        exercise(database_engine)
