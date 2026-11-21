# OpenRDS

OpenRDS is an MIT-licensed database control plane for a single Linux VPS. Install it on your server, open the dashboard, and provision PostgreSQL or MySQL with a persistent data volume, a private endpoint, and generated credentials.

## Install on a VPS

You need Git, **Docker Engine 28+** (rootful, local `/var/run/docker.sock`) and the **Docker Compose plugin 2.20+**. [Docker’s Linux installation instructions](https://docs.docker.com/engine/install/) cover supported distributions. Size the VPS for your engines, data and local backups.

```sh
git clone https://github.com/kerjakuyco/openrds.git
cd openrds
sh install.sh
docker compose exec openrds cat /var/lib/openrds/admin-token
```

The installer builds the dashboard from this checkout and waits for it to start. The admin token is generated on first launch. To choose another dashboard port, copy `.env.example` to `.env` and set `OPENRDS_PORT` before installing.

**From your computer**, open an SSH tunnel (replace `user@your-vps`):

```sh
ssh -N -L 8080:127.0.0.1:8080 user@your-vps
```

Visit **http://127.0.0.1:8080** and paste the token. Both the dashboard and database ports are bound to the VPS loopback interface. If you changed the dashboard port, use that port in the tunnel and URL.

## Create and connect

1. Select **New database**, choose PostgreSQL 17 or MySQL 8.4 LTS, and pick a host port. Optional CPU and memory limits apply to that instance’s container.
2. Choose **Inspect → Connection → Reveal connection URI**. The initial database is `app` and the user is `openrds`.
3. Applications on the VPS can use the URI directly. From another computer, run the SSH command shown in the Connection tab first. For a containerized app, expand **Connect an application container** to get the exact network command and internal URI.

Each instance gets a separate Compose project, network, volume, and random password. Stop/Start preserves the volume. Delete removes that instance’s volume; saved backups have their own lifecycle.

The initial PostgreSQL role administers its isolated cluster; the MySQL user has privileges on `app`. You can create narrower roles for application workloads using your usual SQL client.

## Operate and recover

| Dashboard area | What you can do |
| --- | --- |
| **Overview** | See host CPU/RAM capacity, running instances, instances needing attention, backup storage, and recent operations. Counts link to the relevant inventory. |
| **Databases** | Search by name or port, filter by engine or status, inspect live CPU/memory/network/disk readings, read container logs, and start or stop an instance. |
| **Backups** | Create a logical backup from an instance’s Overview tab. Download it, restore it to a **new** name and port, or remove the backup file. |
| **Activity** | Review the last 200 operation events, including starts, completions, and failures. |

Backups cover the **`app` database**: PostgreSQL uses a custom-format `pg_dump`; MySQL uses `mysqldump` with routines, events and triggers. These are logical backups, not whole-server snapshots: additional databases, cluster roles, replication state, and point-in-time recovery are outside their scope. MySQL’s consistent snapshot applies to transactional tables; avoid schema changes during a dump. A restore uses fresh connection credentials and leaves the source and backup intact. If an import fails, the target is marked **Restore incomplete**.

Backups are initially stored **on the same VPS**. Download a copy to independent storage to recover from losing the host. Downloaded `.dump` and `.sql` files can also be restored with standard `pg_restore` and `mysql` tools.

### Where state lives

- `openrds_control` is the Docker volume mounted at `/var/lib/openrds`. It holds the admin token, instance metadata and database passwords, activity history, and completed backups.
- Each database has its own named data volume. Its internal Compose project name is in `instances/<name>.json` in the control volume. Treat this metadata as secret.
- The control plane uses the host Docker socket to operate these projects; its administrator is a trusted VPS operator. This release is for one admin workspace on one host.
- To update: pull your chosen revision and run `sh install.sh` again. Rebuilding the dashboard preserves existing database containers, volumes, and credentials. `docker compose down` in the checkout stops the dashboard; **`down -v` also deletes the control volume**.
- To rotate the admin token and invalidate browser sessions, run `docker compose stop openrds`, `docker compose run --rm --no-deps --entrypoint rm openrds /var/lib/openrds/admin-token`, then `docker compose up -d`. Read the newly generated token as above.

## HTTP API

Use `Authorization: Bearer <admin-token>` for automation. The browser exchanges the token for a 12-hour HTTP-only session cookie. Inventory endpoints omit credentials; the connection endpoint returns them on request.

```sh
TOKEN=$(docker compose exec -T openrds cat /var/lib/openrds/admin-token)
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"orders","engine":"postgres","port":15432,"cpus":1,"memoryMiB":512}' \
  http://127.0.0.1:8080/api/instances
```

| Method | Path (under `/api`) |
| --- | --- |
| GET / POST | `/instances` |
| GET | `/host` — Docker host name, CPU count, memory capacity and engine version |
| GET | `/instances/{name}/connection`, `/instances/{name}/metrics`, `/instances/{name}/logs` |
| POST | `/instances/{name}/start`, `/instances/{name}/stop`, `/instances/{name}/backups` |
| DELETE | `/instances/{name}` |
| GET | `/backups`, `/backups/{id}/download`, `/activity` |
| POST | `/backups/{id}/restore` — body: `{"name":"orders-restored","port":15433}`; engine is inherited |
| DELETE | `/backups/{id}` |

Operations are synchronous; the first provision may take several minutes to pull the image. Only one mutation runs per instance at a time. A failed creation keeps its record: fix the Docker error and choose **Start** to retry, or delete the instance to release its name and port.

## Development

The server uses the Go standard library, embedded HTML/CSS/JavaScript, and the Docker CLI with Compose. No frontend build step or external services are required.

```sh
go test ./...
go vet ./...
go run .
```

Native development also needs the Docker CLI, daemon and Compose plugin for database operations. Local state defaults to `.openrds/` and the listener to `127.0.0.1:8080`; override with `OPENRDS_DATA_DIR` and `OPENRDS_LISTEN`.

CI installs the packaged dashboard on Linux, exercises both engines against real Docker with `python3 scripts/smoke.py`, and runs a desktop/mobile Chrome workflow with `scripts/ui-smoke.cjs`. The SQL check writes a row, restarts the instance, deletes the source, and reads the same row from a restored backup.

[MIT license](LICENSE)
