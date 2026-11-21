# OpenRDS

OpenRDS is an MIT-licensed database control plane for a single Linux VPS. Install it on your server, open the dashboard, and provision PostgreSQL, MySQL, or Valkey with a persistent data volume, a private endpoint, and generated credentials.

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

1. Select **New database**, choose PostgreSQL 17, MySQL 8.4 LTS, or Valkey 9.1, and pick a host port. Under **Resource limits** you can set CPU cores, memory, and a hard limit on the instance’s data volume.
2. Choose **Inspect → Connection → Reveal connection URI**. PostgreSQL and MySQL create the database `app` and user `openrds`. Valkey uses Redis database `0` and the `default` user; any Redis client can connect.
3. Applications on the VPS can use the URI directly. From another computer, run the SSH command shown in the Connection tab first. For a containerized app, expand **Connect an application container** to get the exact network command and internal URI.

Each instance gets a separate Compose project, network, volume, and random password. Stop/Start preserves the volume. Delete removes that instance’s volume; saved backups have their own lifecycle.

The initial PostgreSQL role administers its isolated cluster; the MySQL user has privileges on `app`. You can create narrower roles for application workloads using your usual SQL client. Valkey stores an RDB snapshot and writes it again on a clean shutdown. If you set a memory limit, Valkey’s `maxmemory` is 75% of that limit and it refuses writes instead of evicting keys.

### Limit an instance’s disk usage

Set **Data volume (GiB)** when creating or restoring an instance. For example, `20` (API: `"diskGiB": 20`) gives that database a fixed-size ext4 filesystem. Docker has no portable volume-size setting, so OpenRDS stores a sparse disk image in the `openrds_disk_images` volume, attaches it with a loop device, and mounts it through the normal local volume driver. Writes past the filesystem fail. Usable space is slightly less than the requested size because ext4 keeps its own metadata. The image is a ceiling, not reserved free space. Docker logs, image layers and OpenRDS backups are outside the cap. **Inspect → Measure volume usage** estimates allocated data-file space on demand.

This needs rootful Docker on Linux, because attaching the image uses a short-lived privileged helper (`openrds-disk-helper`). The database container itself stays unprivileged. Each capped database uses one loop device until it is deleted. After a daemon reboot, OpenRDS reattaches those devices when it starts; a database container that restarts first will retry until the device is back. Rootless Docker and daemons that cannot create loop devices show **Not enforceable here** and reject a capped request before creating an instance. Leave the field blank (API: `diskGiB: 0`) for a regular volume.

Stop a capped database and choose **Apply size** to increase or decrease its limit. Growing extends the ext4 filesystem. Shrinking is refused when the data does not fit, and the previous size is kept. The database stays stopped until you start it again. A limit cannot be added to or removed from an existing volume in place: restore a backup into a new instance for that change. Restores inherit the source cap unless you override it; backups from before this feature default to uncapped.

## Operate and recover

| Dashboard area | What you can do |
| --- | --- |
| **Overview** | See host CPU/RAM capacity, running instances, instances needing attention, backup storage, and recent operations. Counts link to the relevant inventory. |
| **Databases** | Search by name or port, filter by engine or status, inspect live CPU/memory/network/disk readings, read container logs, and start or stop an instance. |
| **Backups** | Create a logical backup from an instance’s Overview tab. Download it, restore it to a **new** name and port, or remove the backup file. |
| **Activity** | Review the last 200 operation events, including starts, completions, and failures. |

Backups cover one database: PostgreSQL uses a custom-format `pg_dump` of `app`; MySQL uses `mysqldump` of `app` with routines, events and triggers; Valkey writes an RDB snapshot. These are logical backups, not whole-server snapshots: additional databases, cluster roles, replication state, and point-in-time recovery are outside their scope. MySQL’s consistent snapshot applies to transactional tables; avoid schema changes during a dump. A restore uses fresh connection credentials and leaves the source and backup intact. Valkey is stopped while the snapshot replaces its data files, then started so it loads that snapshot. If an import fails, the target is marked **Restore incomplete**.

Backups are initially stored **on the same VPS**. Download a copy to independent storage to recover from losing the host. Downloaded `.dump`, `.sql`, and `.rdb` files can also be restored with standard `pg_restore`, `mysql`, and Valkey or Redis tools.

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
| GET | `/host` — Docker host name, CPU count, memory capacity, engine version and `volumeQuota.state` (`supported`, `unsupported` or `unknown`) |
| GET | `/instances/{name}/connection`, `/instances/{name}/metrics`, `/instances/{name}/logs`, `/instances/{name}/storage` (on-demand `usedBytes` for a running instance) |
| POST | `/instances/{name}/start`, `/instances/{name}/stop`, `/instances/{name}/backups` |
| POST | `/instances/{name}/disk` — body: `{"diskGiB": 30}`; stopped capped instance only |
| DELETE | `/instances/{name}` |
| GET | `/backups`, `/backups/{id}/download`, `/activity` |
| POST | `/backups/{id}/restore` — body: `{"name":"orders-restored","port":15433}`; engine and data-volume limit are inherited. Set `diskGiB` to override the limit. |
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

CI installs the packaged dashboard on Linux, exercises PostgreSQL, MySQL, and Valkey against real Docker with `python3 scripts/smoke.py`, and runs a desktop/mobile Chrome workflow with `scripts/ui-smoke.cjs`. The check writes a value, restarts the instance, deletes the source, and reads the same value from a restored backup.

[MIT license](LICENSE)
