# User guide

How to create, secure, change, back up and monitor databases with OpenRDS. New to servers? Start with the [install guide](install.md).

**Contents:** [Create and connect](#create-and-connect) · [Engine versions](#engine-versions) · [Access and security](#access-and-security) · [Modify, reboot and protect](#modify-reboot-and-protect) · [Backups and snapshots](#backups-and-snapshots) · [Accounts and roles](#accounts-and-roles) · [Monitor and operate](#monitor-and-operate) · [Where state lives](#where-state-lives)

## Create and connect

1. Choose **Databases → Create database**. Pick PostgreSQL, MySQL or Valkey and an engine version (the stable one is selected; the latest is one click away), and a template: **Production** (daily backups kept 7 days, deletion protection, 2 vCPU · 4 GiB) or **Dev and test** (backups kept 1 day, 0.5 vCPU · 512 MiB). Then adjust the size, data volume limit, host port, backup window, engine parameters and tags. The summary shows how the CPU and memory limits add up against the host before you create anything.
2. Open the database. Its **Connectivity & security** tab shows the connection string to copy, with the password hidden until you show or copy it, and a second one for the internet while public access is on. **Code examples and options** has ready snippets for Node.js, Python, Go, Prisma, Django, Laravel, Java and command-line clients, and lets you connect as another database user. Viewers see a placeholder instead of the password. From your computer, open an SSH tunnel to the port first: `ssh -N -L PORT:127.0.0.1:PORT you@your-server`.
3. PostgreSQL and MySQL create the database `app` and user `openrds`. Valkey uses Redis database `0` and the `default` user; any Redis client can connect.

Each instance gets a separate Compose project, network, volume, and random password. Stop/Start and Reboot preserve the volume, and the database keeps its [engine version](#engine-versions). Delete removes that instance’s volume; snapshots and automated backups have their own lifecycle.

The initial PostgreSQL role administers its isolated cluster; the MySQL user has privileges on `app`. Give applications narrower users from the **Database users** tab (see below). Valkey stores an RDB snapshot and writes it again on a clean shutdown. If you set a memory limit, Valkey’s `maxmemory` is 75% of that limit and, unless you choose another `maxmemory-policy`, it refuses writes instead of evicting keys.

## Engine versions

| Engine | Versions | Selected by default |
| --- | --- | --- |
| PostgreSQL | 19 (latest), 18 (stable), 17, 16, 15 | 18 |
| MySQL | 9.7 LTS (latest), 8.4 LTS (stable) | 8.4 |
| Valkey | 9.2 (latest), 9.1 (stable), 9.0, 8.1, 8.0, 7.2 | 9.1 |

A database keeps its engine version, so nothing upgrades by surprise. To upgrade, take a snapshot and restore it into a new database on a newer version. The restore form offers the snapshot's version or newer, never an older one, because newer versions read backups from older ones but not the other way round. Databases created before versions could be chosen keep the version they started with (PostgreSQL 17, MySQL 8.4, Valkey 9.1). `GET /api/engines` lists the versions.

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

A starting database gets up to 15 minutes to become healthy, enough to replay its write-ahead log after the host lost power. A database that exits while starting, for example over a setting it refuses, fails within seconds instead. Stopping or rebooting gives the database up to 2 minutes to finish writes and flush its caches, and Docker waits as long when the host shuts down, so a busy database is not killed mid-write. PostgreSQL gets a `/dev/shm` as large as its memory limit (1 GiB without one), which parallel queries on large tables need: Docker's 64 MB default makes them fail with `could not resize shared memory segment`. Databases created before these settings existed get them at their next start or reboot.

Turn on **deletion protection** to make delete fail until it is turned off. When you delete a database you can take a **final snapshot** first; it needs a running database, and if it fails nothing is deleted.

### Engine parameters

Each engine offers a checked list of settings, such as `shared_buffers`, `work_mem`, `max_connections` and `shared_preload_libraries` for PostgreSQL; `innodb_buffer_pool_size`, `slow_query_log` and `sql_mode` for MySQL; `maxmemory-policy`, `save` and `notify-keyspace-events` for Valkey. `GET /api/engines` and the parameter editor list every setting with its type, range, default and description. Values are validated before they reach the database command line, and a cache size (`shared_buffers`, `innodb_buffer_pool_size`) must be smaller than the memory limit.

For heavy workloads, PostgreSQL also offers autovacuum settings, `wal_compression`, `checkpoint_completion_target`, `synchronous_commit`, parallel worker limits, `hash_mem_multiplier`, `track_io_timing` and `idle_session_timeout`; MySQL offers `innodb_io_capacity_max`, `innodb_log_buffer_size`, `innodb_parallel_read_threads`, the per-query `sort_buffer_size` and `join_buffer_size`, and `max_execution_time`; Valkey offers `activedefrag` and `maxmemory-eviction-tenacity`.

**Memory settings follow the memory limit**, as RDS sizes them to the instance class, so a larger instance uses its memory without tuning:

| Engine | Setting | With a memory limit |
| --- | --- | --- |
| PostgreSQL | `shared_buffers` | 25% of the limit |
| PostgreSQL | `effective_cache_size` | 75% of the limit |
| PostgreSQL | `maintenance_work_mem` | 1/16 of the limit, from 64MB to 2GB |
| MySQL | `innodb_buffer_pool_size` and `innodb_buffer_pool_instances` | 75% of the limit less 256 MiB for the server, in one instance per GiB up to 8, rounded down to whole 128 MiB chunks per instance so MySQL never rounds it up |
| Valkey | `maxmemory` | 75% of the limit |

A value you set yourself always wins; if you set `innodb_buffer_pool_size`, MySQL chooses its own instance count. Without a memory limit the engine defaults stay, because OpenRDS cannot know how much of the host a database may use. **Configuration** shows the sized values, and they change with the memory limit at the next restart.

Parameters are passed on the command line from a generated Compose override, so they apply when the container starts. **Configuration** lists each one as applied or pending a restart. Valkey’s append-only persistence is not offered: when AOF is switched on through a restart, Valkey starts from an empty append-only file instead of its RDB snapshot. Restores keep the snapshot’s parameters unless you change them.

### Limit an instance’s disk usage

Set **Data volume limit (GiB)** when creating or restoring an instance. For example, `20` (API: `"diskGiB": 20`) gives that database a fixed-size ext4 filesystem. Docker has no portable volume-size setting, so OpenRDS stores a sparse disk image in the `openrds_disk_images` volume, attaches it with a loop device, and mounts it through the normal local volume driver. Writes past the filesystem fail. Usable space is slightly less than the requested size because ext4 keeps its own metadata. The image is a ceiling, not reserved free space. Docker logs, image layers and OpenRDS backups are outside the cap. **Configuration → Measure usage** estimates allocated data-file space on demand.

This needs rootful Docker on Linux, because attaching the image uses a short-lived privileged helper (`openrds-disk-helper`). The database container itself stays unprivileged. Each capped database uses one loop device until it is deleted. After a server or Docker restart, OpenRDS reattaches those devices when it starts. Docker may try a capped database before that and give up when its disk is missing; OpenRDS then starts it and records **Start database after Docker restart** in Events. Databases you stopped yourself stay stopped. Rootless Docker and daemons that cannot create loop devices show **Limits not enforceable here** and reject a capped request before creating an instance. Leave the field blank (API: `diskGiB: 0`) for a regular volume.

Stop a capped database and choose **Configuration → Storage → Resize** to increase or decrease its limit. Growing extends the ext4 filesystem. Shrinking is refused when the data does not fit, and the previous size is kept. The database stays stopped until you start it again. A limit cannot be added to or removed from an existing volume in place: restore a backup into a new instance for that change. Restores inherit the source cap unless you override it; backups from before this feature default to uncapped.

## Backups and snapshots

- **Automated backups** run once a day during each database’s backup window (UTC, at least 30 minutes long) and are kept for its retention period of 1–35 days. The scheduler checks every minute. A database that is stopped, busy or unhealthy for the whole window misses that day’s backup, and the dashboard recommends checking any database without a backup from the last day. Automated backups expire after their retention period, even when their database has been deleted. A new retention period applies to the database’s existing automated backups; turning automated backups off keeps the existing ones until they expire. The console suggests a free half-hour window for each new database so databases on one host do not all back up at once; an API request without `backupWindow` uses 03:00–03:30.
- **Snapshots** are manual: **Actions → Take snapshot** on a running database, or a final snapshot at deletion. They stay until you delete them. **Keep** turns an automated backup into a snapshot that never expires.

Backups cover one database: PostgreSQL uses a custom-format `pg_dump` of `app`; MySQL uses `mysqldump` of `app` with routines, events and triggers; Valkey writes an RDB snapshot. These are logical backups, not whole-server snapshots: additional databases, cluster roles, replication state, and point-in-time recovery are outside their scope. MySQL’s consistent snapshot applies to transactional tables; avoid schema changes during a dump. **Restore** always creates a **new** database with fresh credentials and leaves the source and backup intact; the form starts from the source’s size, data volume limit and parameters. Valkey is stopped while the snapshot replaces its data files, then started so it loads that snapshot. If an import fails, the target is marked **Restore incomplete**.

MySQL dumps are plain SQL, so OpenRDS stores them gzip-compressed, usually several times smaller; PostgreSQL's custom format and Valkey's RDB files are compressed by the engine already. Backups taken before compression existed stay as they are and restore as before.

A snapshot, restore or import of a large database can take hours. It keeps running when you close the browser or the SSH tunnel drops, for up to 24 hours, and **Events** shows how it ended.

Backups are stored **on the same VPS**, usually on the disk the databases use, and a full disk stops every database on it. So a backup does not start unless the disk keeps a reserve free after it (5% of the disk, from 2 GiB to 10 GiB), judged by the size of the database's previous backup, and a backup that runs the free space down to the reserve stops and is removed. The dashboard warns when free space gets close. Connect **backup storage** (below), or download a copy to independent storage, to recover from losing the host. Downloaded `.dump`, `.sql.gz` (older MySQL backups: `.sql`) and `.rdb` files can also be restored with standard `pg_restore`, `gunzip` with `mysql`, and Valkey or Redis tools.

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
| **Dashboard** | On a new server, pick an engine to create your first database. Afterwards, see your databases with live CPU and memory, the next getting-started step, recommendations (unhealthy databases, backups off or missing, low disk space for backups, pending restarts, unprotected production databases, open or unencrypted public databases), resources with CPU and memory allocation, and recent activity. |
| **Databases** | Filter by name, port, tag, engine or status; sort by any column; select a database and use **Modify** or **Actions** (start, stop, reboot, take snapshot, delete). Each database page follows the RDS console: a summary, then tabs for connectivity and security, database users, monitoring, logs and events, configuration and parameters, maintenance and backups, and tags. |
| **Snapshots** and **Automated backups** | Restore, download, keep or delete recovery points. Automated backups shows how many databases back up daily and one row per database with its schedule, last and next backup, and a **Turn on** or **Change** button that edits the schedule in place. |
| **Events** | Review the last 1,000 operations with who ran them and failure details, filtered by database or user, outcome or text. The sidebar counts new failures since your last visit. |
| **Users** and **Account** | Admins search and filter accounts, invite people, change a role straight from the list, resend pending invites, reset passwords, and disable or delete accounts. Everyone can rename themselves, change their password and sign out other sessions. |

The **Monitoring** tab charts CPU utilization (as a share of the CPU limit, or of the host when unlimited), memory in use against the limit, and network and disk throughput over the last 1, 3, 6, 12 or 24 hours. Every 30 seconds (`OPENRDS_METRICS_INTERVAL`, 5s–5m) the control plane reads each running database’s container statistics from the Docker Engine API through the mounted socket. The Docker CLI rounds statistics to three significant digits, which would make long-running throughput coarse. Readings are kept for 24 hours in memory and saved to `metrics.json` every five minutes and at shutdown, so an update keeps the charts. Each chart has a table view and a keyboard-readable crosshair.

The sidebar groups the console into databases, backups, activity and administration, with a quick link and live status dot for each database. Collapse it to icons on a desktop; on a phone it opens as a drawer. The account menu at its foot holds account settings, the light, dark or system theme, and sign-out.

## Where state lives

- `openrds_control` is the Docker volume mounted at `/var/lib/openrds`. It holds the API key (`admin-token`), accounts (`users.json`, with password hashes), instance metadata and database passwords, backup storage settings (`offsite.json`), the TLS certificate authority and database certificates (`tls/`), generated Compose overrides, activity history, metric history, and completed backups.
- Each database has its own named data volume. Its internal Compose project name is in `instances/<name>.json` in the control volume. Treat this metadata as secret.
- The control plane uses the host Docker socket to operate these projects and read their statistics; anyone with shell access to the server, the API key or an administrator account is a trusted operator. This release manages one host.
- To update: run the install command again, or pull your chosen revision and run `sh install.sh`. Rebuilding the dashboard preserves existing database containers, volumes, and credentials. Existing databases keep automated backups off until you set a retention period. `docker compose down` in the checkout stops the dashboard; **`down -v` also deletes the control volume**.
- Sessions are signed with the API key. To rotate it and sign everyone out, run `docker compose stop openrds`, `docker compose run --rm --no-deps --entrypoint rm openrds /var/lib/openrds/admin-token`, then `docker compose up -d`.
