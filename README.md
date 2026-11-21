# OpenRDS

OpenRDS is an MIT-licensed database control plane for a single Linux VPS. Install it on your server, open the dashboard, and provision PostgreSQL, MySQL, or Valkey with a persistent data volume, a private endpoint, and generated credentials. Each database gets daily automated backups, engine parameters, resource limits you can change later, deletion protection, and monitoring charts, managed from a console modeled on managed-database services.

## Install on a VPS

You need Git, **Docker Engine 28+** (rootful, local `/var/run/docker.sock`) and the **Docker Compose plugin 2.20+**. [Docker’s Linux installation instructions](https://docs.docker.com/engine/install/) cover supported distributions. Size the VPS for your engines, data and local backups.

```sh
git clone https://github.com/kerjakuyco/openrds.git
cd openrds
sh install.sh
docker compose exec openrds cat /var/lib/openrds/admin-token
```

The installer builds the dashboard from this checkout and waits for it to start. The admin token is generated on first launch. To choose another dashboard port or monitoring interval, copy `.env.example` to `.env` and set `OPENRDS_PORT` or `OPENRDS_METRICS_INTERVAL` before installing.

**From your computer**, open an SSH tunnel (replace `user@your-vps`):

```sh
ssh -N -L 8080:127.0.0.1:8080 user@your-vps
```

Visit **http://127.0.0.1:8080** and paste the token. Both the dashboard and database ports are bound to the VPS loopback interface. If you changed the dashboard port, use that port in the tunnel and URL.

## Create and connect

1. Choose **Databases → Create database**. Pick PostgreSQL 17, MySQL 8.4 LTS, or Valkey 9.1, and a template: **Production** (daily backups kept 7 days, deletion protection, 2 vCPU · 4 GiB) or **Dev and test** (backups kept 1 day, 0.5 vCPU · 512 MiB). Then adjust the size, data volume limit, host port, backup window, engine parameters and tags. The summary shows how the CPU and memory limits add up against the host before you create anything.
2. Open the database and choose **Connectivity & security → Reveal connection URI**. PostgreSQL and MySQL create the database `app` and user `openrds`. Valkey uses Redis database `0` and the `default` user; any Redis client can connect.
3. Applications on the VPS can use the URI directly. From another computer, run the SSH command shown on the same tab first. For a containerized app, the tab also shows the network to join and the internal URI.

Each instance gets a separate Compose project, network, volume, and random password. Stop/Start and Reboot preserve the volume. Delete removes that instance’s volume; snapshots and automated backups have their own lifecycle.

The initial PostgreSQL role administers its isolated cluster; the MySQL user has privileges on `app`. You can create narrower roles for application workloads using your usual SQL client. Valkey stores an RDB snapshot and writes it again on a clean shutdown. If you set a memory limit, Valkey’s `maxmemory` is 75% of that limit and, unless you choose another `maxmemory-policy`, it refuses writes instead of evicting keys.

## Modify, reboot and protect

**Modify** changes the CPU and memory limits, host port, backup retention and window, engine parameters, deletion protection and tags. It ends with a summary of modifications showing each attribute’s current and new value:

- Retention, window, deletion protection and tags apply at once, without a restart.
- CPU, memory and engine parameters need a new container. For a running database, choose **Apply during the next restart** (Reboot, or Stop and Start) or **Apply immediately**, which restarts it now. A pending change shows on the database page until it applies; a stopped database applies changes when it starts.
- A port change always applies immediately and restarts a running database, as in RDS.

If an immediate change cannot start, for example because the new port is used by another program on the VPS, OpenRDS restores the previous settings, restarts the database with them, and reports why.

Turn on **deletion protection** to make delete fail until it is turned off. When you delete a database you can take a **final snapshot** first; it needs a running database, and if it fails nothing is deleted.

### Engine parameters

Each engine offers a checked list of settings, such as `shared_buffers`, `work_mem`, `max_connections` and `shared_preload_libraries` for PostgreSQL; `innodb_buffer_pool_size`, `slow_query_log` and `sql_mode` for MySQL; `maxmemory-policy`, `save` and `notify-keyspace-events` for Valkey. `GET /api/engines` and the parameter editor list every setting with its type, range, default and description. Values are validated before they reach the database command line, and a cache size (`shared_buffers`, `innodb_buffer_pool_size`) must be smaller than the memory limit.

Parameters are passed on the command line from a generated Compose override, so they apply when the container starts. **Configuration** lists each one as applied or pending a restart. Valkey’s append-only persistence is not offered: when AOF is switched on through a restart, Valkey starts from an empty append-only file instead of its RDB snapshot. Restores keep the snapshot’s parameters unless you change them.

### Limit an instance’s disk usage

Set **Data volume limit (GiB)** when creating or restoring an instance. For example, `20` (API: `"diskGiB": 20`) gives that database a fixed-size ext4 filesystem. Docker has no portable volume-size setting, so OpenRDS stores a sparse disk image in the `openrds_disk_images` volume, attaches it with a loop device, and mounts it through the normal local volume driver. Writes past the filesystem fail. Usable space is slightly less than the requested size because ext4 keeps its own metadata. The image is a ceiling, not reserved free space. Docker logs, image layers and OpenRDS backups are outside the cap. **Configuration → Measure volume usage** estimates allocated data-file space on demand.

This needs rootful Docker on Linux, because attaching the image uses a short-lived privileged helper (`openrds-disk-helper`). The database container itself stays unprivileged. Each capped database uses one loop device until it is deleted. After a daemon reboot, OpenRDS reattaches those devices when it starts; a database container that restarts first will retry until the device is back. Rootless Docker and daemons that cannot create loop devices show **Limits not enforceable here** and reject a capped request before creating an instance. Leave the field blank (API: `diskGiB: 0`) for a regular volume.

Stop a capped database and choose **Configuration → Apply size** to increase or decrease its limit. Growing extends the ext4 filesystem. Shrinking is refused when the data does not fit, and the previous size is kept. The database stays stopped until you start it again. A limit cannot be added to or removed from an existing volume in place: restore a backup into a new instance for that change. Restores inherit the source cap unless you override it; backups from before this feature default to uncapped.

## Backups and snapshots

- **Automated backups** run once a day during each database’s backup window (UTC, at least 30 minutes long) and are kept for its retention period of 1–35 days. The scheduler checks every minute. A database that is stopped, busy or unhealthy for the whole window misses that day’s backup, and the dashboard recommends checking any database without a backup from the last day. Automated backups expire after their retention period, even when their database has been deleted. A new retention period applies to the database’s existing automated backups; turning automated backups off keeps the existing ones until they expire. The console suggests a free half-hour window for each new database so databases on one host do not all back up at once; an API request without `backupWindow` uses 03:00–03:30.
- **Snapshots** are manual: **Actions → Take snapshot** on a running database, or a final snapshot at deletion. They stay until you delete them. **Keep** turns an automated backup into a snapshot that never expires.

Backups cover one database: PostgreSQL uses a custom-format `pg_dump` of `app`; MySQL uses `mysqldump` of `app` with routines, events and triggers; Valkey writes an RDB snapshot. These are logical backups, not whole-server snapshots: additional databases, cluster roles, replication state, and point-in-time recovery are outside their scope. MySQL’s consistent snapshot applies to transactional tables; avoid schema changes during a dump. **Restore** always creates a **new** database with fresh credentials and leaves the source and backup intact; the form starts from the source’s size, data volume limit and parameters. Valkey is stopped while the snapshot replaces its data files, then started so it loads that snapshot. If an import fails, the target is marked **Restore incomplete**.

Backups are stored **on the same VPS**. Download a copy to independent storage to recover from losing the host. Downloaded `.dump`, `.sql`, and `.rdb` files can also be restored with standard `pg_restore`, `mysql`, and Valkey or Redis tools.

## Monitor and operate

| Console area | What you can do |
| --- | --- |
| **Dashboard** | See database, snapshot and backup counts, recommendations (unhealthy databases, backups off or missing, pending restarts, unprotected databases tagged `env=production`, databases without a memory limit), host CPU and memory allocation, and recent events. |
| **Databases** | Filter by name, port, tag, engine or status; sort by any column; select a database and use **Modify** or **Actions** (start, stop, reboot, take snapshot, delete). Each database has tabs for connectivity, monitoring, logs and events, configuration and parameters, maintenance and backups, and tags. |
| **Snapshots** and **Automated backups** | Restore, download, keep or delete recovery points, and review each database’s backup schedule, latest backup and next backup. |
| **Events** | Review the last 1,000 operations, including failure details, filtered by database, outcome or text. |

The **Monitoring** tab charts CPU utilization (as a share of the CPU limit, or of the host when unlimited), memory in use against the limit, and network and disk throughput over the last 1, 3, 6, 12 or 24 hours. Every 30 seconds (`OPENRDS_METRICS_INTERVAL`, 5s–5m) the control plane reads each running database’s container statistics from the Docker Engine API through the mounted socket. The Docker CLI rounds statistics to three significant digits, which would make long-running throughput coarse. Readings are kept for 24 hours in memory and saved to `metrics.json` every five minutes and at shutdown, so an update keeps the charts. Each chart has a table view and a keyboard-readable crosshair. The console follows the system light or dark theme unless you pick one.

### Where state lives

- `openrds_control` is the Docker volume mounted at `/var/lib/openrds`. It holds the admin token, instance metadata and database passwords, generated Compose overrides for engine parameters, activity history, metric history, and completed backups.
- Each database has its own named data volume. Its internal Compose project name is in `instances/<name>.json` in the control volume. Treat this metadata as secret.
- The control plane uses the host Docker socket to operate these projects and read their statistics; its administrator is a trusted VPS operator. This release is for one admin workspace on one host.
- To update: pull your chosen revision and run `sh install.sh` again. Rebuilding the dashboard preserves existing database containers, volumes, and credentials. Existing databases keep automated backups off until you set a retention period. `docker compose down` in the checkout stops the dashboard; **`down -v` also deletes the control volume**.
- To rotate the admin token and invalidate browser sessions, run `docker compose stop openrds`, `docker compose run --rm --no-deps --entrypoint rm openrds /var/lib/openrds/admin-token`, then `docker compose up -d`. Read the newly generated token as above.

## HTTP API

Use `Authorization: Bearer <admin-token>` for automation. The browser exchanges the token for a 12-hour HTTP-only session cookie. Inventory endpoints omit credentials; the connection endpoint returns them on request.

```sh
TOKEN=$(docker compose exec -T openrds cat /var/lib/openrds/admin-token)
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"orders","engine":"postgres","port":15432,"cpus":1,"memoryMiB":1024,
       "backupRetentionDays":7,"backupWindow":"03:00-03:30","deletionProtection":true,
       "tags":{"env":"production"},"parameters":{"max_connections":"200"}}' \
  http://127.0.0.1:8080/api/instances
```

Only `name`, `engine` and `port` are required. Omitted limits mean unlimited, and an omitted `backupRetentionDays` means automated backups are off.

| Method | Path (under `/api`) |
| --- | --- |
| GET / POST | `/instances` |
| GET | `/engines` — engines, suggested ports, memory minimums and adjustable parameters |
| GET | `/host` — Docker host name, CPU count, memory capacity, engine version and `volumeQuota.state` (`supported`, `unsupported` or `unknown`) |
| GET | `/instances/{name}/connection`, `/instances/{name}/metrics`, `/instances/{name}/logs`, `/instances/{name}/storage` (on-demand `usedBytes` for a running instance) |
| GET | `/instances/{name}/history?range=1h` — readings for `1h`, `3h`, `6h`, `12h` or `24h`, averaged to about 240 points |
| POST | `/instances/{name}/start`, `/instances/{name}/stop`, `/instances/{name}/reboot`, `/instances/{name}/backups` |
| POST | `/instances/{name}/modify` — any of `cpus`, `memoryMiB`, `port`, `backupRetentionDays`, `backupWindow`, `deletionProtection`, `tags`, `parameters`, plus `applyImmediately`; returns `{"restarted": …, "pendingRestart": …}`. Tags and parameters are replaced as a whole. |
| POST | `/instances/{name}/disk` — body: `{"diskGiB": 30}`; stopped capped instance only |
| DELETE | `/instances/{name}` — add `?finalSnapshot=true` to take a final snapshot first; refused while deletion protection is on |
| GET | `/backups`, `/backups/{id}/download`, `/activity` |
| POST | `/backups/{id}/restore` — body: `{"name":"orders-restored","port":15433}`; engine, data-volume limit and parameters are inherited. Set `diskGiB` or `parameters` to override them. |
| POST | `/backups/{id}/keep` — turn an automated backup into a snapshot that never expires |
| DELETE | `/backups/{id}` |

Backups report `kind` (`manual` or `automated`), `expiresAt` for automated backups, and `final` for final snapshots. Operations are synchronous; the first provision may take several minutes to pull the image. Only one mutation runs per instance at a time. A failed creation keeps its record: fix the Docker error and choose **Start** to retry, or delete the instance to release its name and port.

## Development

The server uses the Go standard library, the Docker CLI with Compose, and the Docker Engine API over the local socket for statistics. The console is plain HTML, CSS and JavaScript modules embedded in the binary. There is no frontend build step and no external service.

```sh
go test ./...
go vet ./...
go run .
```

Native development also needs the Docker CLI, daemon and Compose plugin for database operations. Local state defaults to `.openrds/` and the listener to `127.0.0.1:8080`; override with `OPENRDS_DATA_DIR` and `OPENRDS_LISTEN`. Metric history needs a unix Docker socket (`/var/run/docker.sock`, or `DOCKER_HOST=unix://…`); elsewhere the Monitoring tab shows live readings instead.

CI installs the packaged dashboard on Linux and exercises PostgreSQL, MySQL, and Valkey against real Docker with `python3 scripts/smoke.py`: parameters, a deferred change applied by reboot, metric history, backups, deletion protection, and a final snapshot restored into a new instance. A desktop and mobile Chrome workflow, `scripts/ui-smoke.cjs`, then creates a database with parameters and tags, walks every tab, takes a snapshot, modifies and reboots it, and checks the restore form, events, themes and mobile layouts.

[MIT license](LICENSE)
