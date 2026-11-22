# OpenRDS

OpenRDS is an MIT-licensed database control plane for a single Linux VPS. Install it on your server, open the dashboard, and provision PostgreSQL, MySQL, or Valkey with a persistent data volume, a private endpoint, and generated credentials. Each database gets daily automated backups, engine parameters, resource limits you can change later, deletion protection, and monitoring charts, managed from a console modeled on managed-database services. Teammates sign in with their own accounts and roles, and a getting-started checklist and connection guide help newcomers from an empty VPS to a connected application.

## Install on a VPS

On a Linux server, run one command:

```sh
curl -fsSL https://raw.githubusercontent.com/kerjakuyco/openrds/main/install.sh | sh
```

The installer downloads OpenRDS into `~/openrds`, offers to install Docker with Docker's official script if it is missing, builds and starts the dashboard, and then prints what to open. It continues with `sudo` when your user cannot use Docker. Already have a copy? Run `sh install.sh` inside it; running either command again updates OpenRDS and keeps your databases, backups and accounts. Set `OPENRDS_YES=1` to accept its questions, or `OPENRDS_DIR` to choose another folder.

OpenRDS needs **Docker Engine 28+** (rootful, with the local `/var/run/docker.sock`) and the **Docker Compose plugin**. Size the server for your engines, data and local backups. To choose another dashboard port or monitoring interval, copy `.env.example` to `.env` and set `OPENRDS_PORT` or `OPENRDS_METRICS_INTERVAL` before installing.

The dashboard and databases listen only on the server itself. **From your computer**, open the SSH tunnel the installer prints, for example:

```sh
ssh -N -L 8080:127.0.0.1:8080 user@your-server
```

Then visit **http://127.0.0.1:8080** and create your administrator account; there is no token to find or paste. Until that first account exists, whoever opens the dashboard can create it, which means someone with SSH access to the server. If you changed the dashboard port, use that port in the tunnel and URL.

**On your own computer**, for a quick look, install Docker Desktop and run `sh install.sh` from a clone, for example in Git Bash on Windows. It opens at http://127.0.0.1:8080 without a tunnel. Data-volume limits need Linux loop devices, so the dashboard may report them as not enforceable there.

## Create and connect

1. Choose **Databases → Create database**. Pick PostgreSQL 17, MySQL 8.4 LTS, or Valkey 9.1, and a template: **Production** (daily backups kept 7 days, deletion protection, 2 vCPU · 4 GiB) or **Dev and test** (backups kept 1 day, 0.5 vCPU · 512 MiB). Then adjust the size, data volume limit, host port, backup window, engine parameters and tags. The summary shows how the CPU and memory limits add up against the host before you create anything.
2. Open the database. Its **Connectivity & security** tab shows the connection string to copy, with the password hidden until you show or copy it, and a second one for the internet while public access is on. **Code examples and options** has ready snippets for Node.js, Python, Go, Prisma, Django, Laravel, Java and command-line clients, and lets you connect as another database user. Viewers see a placeholder instead of the password. From your computer, open an SSH tunnel to the port first: `ssh -N -L PORT:127.0.0.1:PORT you@your-server`.
3. PostgreSQL and MySQL create the database `app` and user `openrds`. Valkey uses Redis database `0` and the `default` user; any Redis client can connect.

Each instance gets a separate Compose project, network, volume, and random password. Stop/Start and Reboot preserve the volume. Delete removes that instance’s volume; snapshots and automated backups have their own lifecycle.

The initial PostgreSQL role administers its isolated cluster; the MySQL user has privileges on `app`. Give applications narrower users from the **Database users** tab (see below). Valkey stores an RDB snapshot and writes it again on a clean shutdown. If you set a memory limit, Valkey’s `maxmemory` is 75% of that limit and, unless you choose another `maxmemory-policy`, it refuses writes instead of evicting keys.

## Access and security

Every database starts private: its port is published on the VPS loopback interface only, so it is reachable from the VPS, from its Docker containers and through SSH tunnels, and nothing listens publicly.

- **Database users.** The **Database users** tab lists the owner (`openrds`, or `default` for Valkey) and extra users. Create one per application with **Read and write** (change rows or keys, no schema or administrative commands) or **Read only** access. PostgreSQL users get the built-in `pg_read_all_data` and `pg_write_all_data` roles; MySQL users get `SELECT, SHOW VIEW` (plus `INSERT, UPDATE, DELETE`) on `app`; Valkey users get ACL rules that also block dangerous commands such as `FLUSHALL`, `KEYS` and `CONFIG`. Deleting a user ends its open sessions. Snapshots carry data, not users.
- **Change passwords.** Change the owner's or a user's password at any time, generated (recommended) or your own (16–128 letters, digits and `. _ ~ -`). The old password stops working at once. Passwords travel to the engine on stdin, never on a command line; PostgreSQL receives only a SCRAM-SHA-256 verifier, and Valkey only SHA-256 digests. Every password reveal and change is recorded in **Events**.
- **Encryption in transit.** Each installation has its own certificate authority, which signs a certificate for every database, valid for `localhost`, `127.0.0.1`, the container name and the public address. Download the CA certificate from the Connectivity tab and use `sslmode=verify-full` (PostgreSQL), `ssl-mode=VERIFY_IDENTITY` (MySQL) or `rediss://` (Valkey). TLS is always available for PostgreSQL and MySQL; **Require TLS** under Modify refuses plain-text connections. Valkey serves either TLS or plain text on a port, so requiring TLS switches it to TLS only.
- **Public access and firewall.** Under **Network access**, turn on public access to open a second port on every interface, choose the sources allowed to connect (addresses or ranges such as `203.0.113.7` or `203.0.113.0/24`), and set the public address clients use. A small gateway container, built from the OpenRDS image, forwards only allowed sources; Docker publishes ports ahead of host firewalls such as `ufw`, so these rules are what protects the port. **Recent connection attempts** lists who tried, with one-click **Allow** for a blocked address. Rule changes apply at once. Turn on Require TLS before opening a database to the internet; the dashboard warns when a public database allows everyone or does not require TLS.

Databases created by earlier versions get TLS, password changes and database users after one reboot.

## Modify, reboot and protect

For one change at a time, use the database's **Configuration** tab: each setting (instance size, host port, storage, automated backups, Require TLS, deletion protection and engine parameters) has its own **Change** button or switch, and changes that need a restart ask whether to apply now or at the next restart.

**Modify** changes the CPU and memory limits, host port, backup retention and window, engine parameters, deletion protection, Require TLS and tags. It ends with a summary of modifications showing each attribute’s current and new value:

- Retention, window, deletion protection and tags apply at once, without a restart.
- CPU, memory, engine parameters and Require TLS need a new container. For a running database, choose **Apply during the next restart** (Reboot, or Stop and Start) or **Apply immediately**, which restarts it now. A pending change shows on the database page until it applies; a stopped database applies changes when it starts.
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

Backups are stored **on the same VPS**. Connect **backup storage** (below), or download a copy to independent storage, to recover from losing the host. Downloaded `.dump`, `.sql`, and `.rdb` files can also be restored with standard `pg_restore`, `mysql`, and Valkey or Redis tools.

### Backup storage (S3-compatible)

Under **Backups → Backup storage**, an admin connects any S3-compatible object storage: Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, MinIO or another service. Choose the provider to fill in its usual endpoint, region and URL style, then enter the bucket, an optional folder and an access key that can read, write, list and delete in the bucket. **Test connection** writes, reads and deletes a small object; settings are only saved when that works. Create the bucket first and keep it private: copies are logical dumps of your data, not encrypted by OpenRDS, so rely on the provider's encryption at rest.

- Every backup, automated or snapshot, is copied to `<folder>/<backup id>/data` and `meta.json` right after it is taken; existing backups are copied when you connect. Large backups upload in parts. Failed copies are retried every 10 minutes, and the dashboard warns about them.
- Copies follow the library: deleting a backup, or its expiry, deletes its copy.
- **Backups in storage** lists every copy in the folder, including copies from other servers. After losing a server, install OpenRDS on a new one, connect the same bucket and folder, and **Import** a copy: it becomes a snapshot you can restore.
- **Disconnect** stops copying and forgets the keys; copies already in the bucket stay.

Requests are signed with AWS Signature Version 4 and carry the SHA-256 of their payload. The secret key is kept in the control volume and never returned by the API.

## Accounts and roles

Everyone signs in with their own username and password. Each account has a role:

| Role | Can |
| --- | --- |
| **Admin** | Everything, including inviting people and changing roles. |
| **Operator** | Create, modify, start, stop and delete databases; take, restore, download and delete backups; manage database users, passwords and network access; reveal database passwords. |
| **Viewer** | Read-only: databases, monitoring, logs, backups and events. No passwords, no backup downloads, no changes. |

The server enforces the role on every API request; the console hides what a role cannot do. An admin invites someone from **Users**. OpenRDS returns a one-time link that expires after 72 hours, and the person chooses their own password with it, so the admin never handles it. **Reset password** signs the account out everywhere and issues a new link. Disabling an account signs it out at once. You cannot change your own role, disable or delete yourself, or leave OpenRDS without an active administrator.

Passwords have at least 10 characters and are stored as PBKDF2-SHA256 hashes with 600,000 iterations. Five failed sign-ins in a row lock that username for 30 seconds, doubling up to 15 minutes. A session lasts 12 hours. Changing your password, or **Account → Sign out other sessions**, ends the account's other sessions. Events record who did what: an account name, `command line` for the reset command below, `API key` for scripts, or OpenRDS itself for scheduled work.

**Forgot your password?** Another administrator can send you a reset link from **Users**. If you are the only administrator, run this on the server, in the OpenRDS folder, and open the link it prints:

```sh
docker compose exec openrds openrds reset-password YOUR_USERNAME
```

Accounts decide who can do what once connected; the network decides who can connect. The console still listens only on the VPS, so each person reaches it through their own SSH tunnel, or through a TLS reverse proxy you put in front of it.

## Monitor and operate

| Console area | What you can do |
| --- | --- |
| **Getting started** | Follow a checklist that ticks itself off: create a database, connect an application, turn on automated backups, keep a copy off the VPS, and invite your team. It also explains the main ideas in plain language. The dashboard shows the next step until you finish or hide it. |
| **Dashboard** | On a new server, pick an engine to create your first database. Afterwards, see your databases with live CPU and memory, the next getting-started step, recommendations (unhealthy databases, backups off or missing, pending restarts, unprotected production databases, open or unencrypted public databases), resources with CPU and memory allocation, and recent activity. |
| **Databases** | Filter by name, port, tag, engine or status; sort by any column; select a database and use **Modify** or **Actions** (start, stop, reboot, take snapshot, delete). Each database page follows the RDS console: a summary, then tabs for connectivity and security, database users, monitoring, logs and events, configuration and parameters, maintenance and backups, and tags. |
| **Snapshots** and **Automated backups** | Restore, download, keep or delete recovery points. Automated backups shows how many databases back up daily and one row per database with its schedule, last and next backup, and a **Turn on** or **Change** button that edits the schedule in place. |
| **Events** | Review the last 1,000 operations with who ran them and failure details, filtered by database or user, outcome or text. The sidebar counts new failures since your last visit. |
| **Users** and **Account** | Admins invite people and change roles. Everyone can rename themselves, change their password and sign out other sessions. |

The **Monitoring** tab charts CPU utilization (as a share of the CPU limit, or of the host when unlimited), memory in use against the limit, and network and disk throughput over the last 1, 3, 6, 12 or 24 hours. Every 30 seconds (`OPENRDS_METRICS_INTERVAL`, 5s–5m) the control plane reads each running database’s container statistics from the Docker Engine API through the mounted socket. The Docker CLI rounds statistics to three significant digits, which would make long-running throughput coarse. Readings are kept for 24 hours in memory and saved to `metrics.json` every five minutes and at shutdown, so an update keeps the charts. Each chart has a table view and a keyboard-readable crosshair.

The sidebar groups the console into databases, backups, activity and administration, with a quick link and live status dot for each database. Collapse it to icons on a desktop; on a phone it opens as a drawer. The account menu at its foot holds account settings, the light, dark or system theme, and sign-out.

### Where state lives

- `openrds_control` is the Docker volume mounted at `/var/lib/openrds`. It holds the API key (`admin-token`), accounts (`users.json`, with password hashes), instance metadata and database passwords, backup storage settings (`offsite.json`), the TLS certificate authority and database certificates (`tls/`), generated Compose overrides, activity history, metric history, and completed backups.
- Each database has its own named data volume. Its internal Compose project name is in `instances/<name>.json` in the control volume. Treat this metadata as secret.
- The control plane uses the host Docker socket to operate these projects and read their statistics; anyone with shell access to the server, the API key or an administrator account is a trusted operator. This release manages one host.
- To update: run the install command again, or pull your chosen revision and run `sh install.sh`. Rebuilding the dashboard preserves existing database containers, volumes, and credentials. Existing databases keep automated backups off until you set a retention period. `docker compose down` in the checkout stops the dashboard; **`down -v` also deletes the control volume**.
- Sessions are signed with the API key. To rotate it and sign everyone out, run `docker compose stop openrds`, `docker compose run --rm --no-deps --entrypoint rm openrds /var/lib/openrds/admin-token`, then `docker compose up -d`.

## HTTP API

Scripts send the API key as `Authorization: Bearer <key>`; it has the Admin role. Print it on the server with `docker compose exec openrds openrds api-key`, and keep it secret. Browsers sign in with an account and get a 12-hour HTTP-only session cookie. Each endpoint needs at least the role in the table: viewer for reading, operator for changes, credentials and backup downloads, and admin for accounts. Inventory endpoints omit credentials; the connection endpoint returns them on request.

```sh
TOKEN=$(docker compose exec -T openrds openrds api-key)
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
| GET | `/instances/{name}/connection` (add `?user=` for a database user; each call is recorded), `/instances/{name}/metrics`, `/instances/{name}/logs`, `/instances/{name}/storage` (on-demand `usedBytes` for a running instance) |
| GET | `/instances/{name}/history?range=1h` — readings for `1h`, `3h`, `6h`, `12h` or `24h`, averaged to about 240 points |
| POST | `/instances/{name}/start`, `/instances/{name}/stop`, `/instances/{name}/reboot`, `/instances/{name}/backups` |
| POST | `/instances/{name}/modify` — any of `cpus`, `memoryMiB`, `port`, `backupRetentionDays`, `backupWindow`, `deletionProtection`, `tags`, `parameters`, `requireTLS`, `publicAccess`, `publicPort`, `publicHost`, `firewall` (`[{"cidr","description"}]`), plus `applyImmediately`; returns `{"restarted": …, "pendingRestart": …}`. Tags and parameters are replaced as a whole. |
| POST | `/instances/{name}/disk` — body: `{"diskGiB": 30}`; stopped capped instance only |
| POST | `/instances/{name}/password` — change the owner password: `{"password": ""}` generates one; returns `{"password"}` |
| POST | `/instances/{name}/users` — `{"name","access","password"}` with `access` `read-only` or `read-write`; returns `{"name","password"}` |
| POST / DELETE | `/instances/{name}/users/{user}/password` (change), `/instances/{name}/users/{user}` (delete and end its sessions) |
| GET | `/instances/{name}/connections` — recent sources that tried the public port; `/tls/ca.pem` — the CA certificate |
| GET / PUT / DELETE | `/storage` — backup storage status (anyone), connect with `{"provider","endpoint","region","bucket","prefix","accessKeyId","secretAccessKey","pathStyle"}` after a test (admin; an empty secret keeps the saved one), or disconnect (admin) |
| POST | `/storage/test` — admin: test settings without saving |
| GET | `/storage/backups` — operator: copies in storage, with `local` when the library also has them |
| POST / DELETE | `/storage/backups/{id}/import` (bring a copy into the library as a snapshot), `/storage/backups/{id}` (delete a copy that is not in the library) |
| DELETE | `/instances/{name}` — add `?finalSnapshot=true` to take a final snapshot first; refused while deletion protection is on |
| GET | `/backups`, `/backups/{id}/download`, `/activity` |
| POST | `/backups/{id}/restore` — body: `{"name":"orders-restored","port":15433}`; engine, data-volume limit and parameters are inherited. Set `diskGiB` or `parameters` to override them. |
| POST | `/backups/{id}/keep` — turn an automated backup into a snapshot that never expires |
| DELETE | `/backups/{id}` |
| GET | `/status` — public: `{"setup": true}` until the first account exists |
| POST | `/setup` — public until the first account exists: `{"username","name","password"}` creates the administrator and signs in |
| POST / GET / DELETE | `/session` — sign in with `{"username","password"}`, read who you are, sign out |
| GET / POST | `/invites/{secret}` — public: read an invite or reset link, or set the password with `{"password"}` |
| GET / POST | `/users` — admin: list accounts, or invite one with `{"username","name","role"}`; returns the link secret once |
| PATCH / DELETE | `/users/{id}` — admin: change `name`, `role` or `disabled`, or delete |
| POST | `/users/{id}/reset` — admin: sign the account out and issue a new link |
| PATCH | `/account` — your `name`; POST `/account/password` with `{"current","password"}`; POST `/account/signout` ends your other sessions |

Roles: viewer for every GET except `/connection` and `/download`, which need operator like all changes; admin for `/users`. Backups report `kind` (`manual` or `automated`), `expiresAt` for automated backups, and `final` for final snapshots. Operations are synchronous; the first provision may take several minutes to pull the image. Only one mutation runs per instance at a time. A failed creation keeps its record: fix the Docker error and choose **Start** to retry, or delete the instance to release its name and port.

## Development

The server uses the Go standard library, the Docker CLI with Compose, and the Docker Engine API over the local socket for statistics. The console is plain HTML, CSS and JavaScript modules embedded in the binary. There is no frontend build step and no external service.

```sh
go test ./...
go vet ./...
go run .
```

Native development also needs the Docker CLI, daemon and Compose plugin for database operations. Local state defaults to `.openrds/` and the listener to `127.0.0.1:8080`; override with `OPENRDS_DATA_DIR` and `OPENRDS_LISTEN`. Metric history needs a unix Docker socket (`/var/run/docker.sock`, or `DOCKER_HOST=unix://…`); elsewhere the Monitoring tab shows live readings instead.

CI installs the packaged dashboard on Linux with `install.sh` and exercises PostgreSQL, MySQL, and Valkey against real Docker with `python3 scripts/smoke.py`: parameters, a deferred change applied by reboot, metric history, backups, deletion protection, and a final snapshot restored into a new instance. It first checks accounts: invitations, sign-in, role limits and the audit trail. A desktop and mobile Chrome workflow, `scripts/ui-smoke.cjs`, then completes first-run setup, creates a database with parameters and tags, walks every tab, takes a snapshot, modifies and reboots it, checks the restore form, events and themes, invites a viewer who accepts the link in a second browser, and checks mobile layouts.

[MIT license](LICENSE)
