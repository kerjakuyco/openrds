# User guide

How to create, secure, change, back up and monitor databases with OpenRDS. New to servers? Start with the [install guide](install.md).

**Contents:** [Create and connect](#create-and-connect) · [Engine versions](#engine-versions) · [Access and security](#access-and-security) · [Modify, reboot and protect](#modify-reboot-and-protect) · [Backups and snapshots](#backups-and-snapshots) · [Accounts and roles](#accounts-and-roles) · [Monitor and operate](#monitor-and-operate) · [Where state lives](#where-state-lives)

## Create and connect

1. Choose **Databases → Create database**. Pick PostgreSQL, MySQL or Valkey and an engine version (the stable one is selected; the latest is one click away), and a template: **Production** (daily backups kept 7 days, deletion protection, 2 vCPU · 4 GiB) or **Dev and test** (backups kept 1 day, 0.5 vCPU · 512 MiB). Then adjust the size, data volume limit, host port, backup window, engine parameters and tags. The summary shows how the CPU and memory limits add up against the host before you create anything.
2. Open the database. Its **Overview** shows the last hour of CPU, memory, connections and throughput, where to connect, replicas, pooling, backups and recent events. The **Connect** tab (or the **Connect** button at the top) shows the connection string to copy, with the password hidden until you show or copy it, and a second one for the internet while public access is on. **Code examples and options** has ready snippets for Node.js, Python, Go, Prisma, Django, Laravel, Java and command-line clients, and lets you connect as another database user. Viewers see a placeholder instead of the password. From your computer, open an SSH tunnel to the port first: `ssh -N -L PORT:127.0.0.1:PORT you@your-server`.
3. PostgreSQL and MySQL create the database `app` and user `openrds`. Valkey uses Redis database `0` and the `default` user; any Redis client can connect.

Each instance gets a separate Compose project, network, volume, and random password. Stop/Start and Reboot preserve the volume, and the database keeps its [engine version](#engine-versions). Delete removes that instance’s volume; snapshots and automated backups have their own lifecycle.

The initial PostgreSQL role administers its isolated cluster; the MySQL user has privileges on `app`. Give applications narrower users from the **Users** tab (see below). Valkey stores an RDB snapshot and writes it again on a clean shutdown. If you set a memory limit, Valkey’s `maxmemory` is 75% of that limit and, unless you choose another `maxmemory-policy`, it refuses writes instead of evicting keys.

## Engine versions

| Engine | Versions | Selected by default |
| --- | --- | --- |
| PostgreSQL | 19 (latest), 18 (stable), 17, 16, 15 | 18 |
| MySQL | 9.7 LTS (latest), 8.4 LTS (stable) | 8.4 |
| Valkey | 9.2 (latest), 9.1 (stable), 9.0, 8.1, 8.0, 7.2 | 9.1 |

A database keeps its engine version, so nothing upgrades by surprise. To move to a newer version, choose **Settings → Engine version → Upgrade**, like RDS's *Modify engine version*. The database keeps its name, port, Docker network name, passwords, users, pooler, public access and settings, so applications need no change:

1. OpenRDS downloads the new version first, so that adds no downtime.
2. It keeps clients out (PostgreSQL accepts only local connections, MySQL goes into offline mode, Valkey pauses writes) and takes a snapshot, noted *Before the upgrade to …*, which is kept.
3. It stops the database and starts the new version on a fresh data volume, with no host port and another network name, restores the snapshot and recreates the database users.
4. It starts the new version on the database's own port and network name, with its gateway and pooler, and removes the previous version's volume.

Clients are disconnected for about as long as a snapshot and a restore of the database take: about 10 seconds for a small one. Writes that arrive meanwhile are refused, never lost after being accepted. If any step fails, the previous version starts again on its untouched volume with all its data, and the snapshot stays. Versions only go up. A database with a disk limit gets a new disk of the same size for the new version. To try a version without touching a database, restore a snapshot into a new one: the restore form offers the snapshot's version or newer, never an older one, because newer versions read backups from older ones but not the other way round. Databases created before versions could be chosen keep the version they started with (PostgreSQL 17, MySQL 8.4, Valkey 9.1). `GET /api/engines` lists the versions.

## Access and security

Every database starts private: its port is published on the VPS loopback interface only, so it is reachable from the VPS, from its Docker containers and through SSH tunnels, and nothing listens publicly.

- **Database users.** The **Users** tab lists the owner (`openrds`, or `default` for Valkey) and extra users. Create one per application with **Read and write** (change rows or keys, no schema or administrative commands) or **Read only** access. PostgreSQL users get the built-in `pg_read_all_data` and `pg_write_all_data` roles; MySQL users get `SELECT, SHOW VIEW` (plus `INSERT, UPDATE, DELETE`) on `app`; Valkey users get ACL rules that also block dangerous commands such as `FLUSHALL`, `KEYS` and `CONFIG`. Deleting a user ends its open sessions. Snapshots carry data, not users.
- **Change passwords.** Change the owner's or a user's password at any time, generated (recommended) or your own (16–128 letters, digits and `. _ ~ -`). The old password stops working at once. Passwords travel to the engine on stdin, never on a command line; PostgreSQL receives only a SCRAM-SHA-256 verifier, and Valkey only SHA-256 digests. Every password reveal and change is recorded in **Events**.
- **Encryption in transit.** Each installation has its own certificate authority, which signs a certificate for every database, valid for `localhost`, `127.0.0.1`, the container name, the public address, and its server's private and public addresses. Download the CA certificate from the Connectivity tab and use `sslmode=verify-full` (PostgreSQL), `ssl-mode=VERIFY_IDENTITY` (MySQL) or `rediss://` (Valkey). TLS is always available for PostgreSQL and MySQL; **Require TLS** under Modify refuses plain-text connections. Valkey serves either TLS or plain text on a port, so requiring TLS switches it to TLS only.
- **Public access and firewall.** Under **Network access**, turn on public access to open a second port on every interface, choose the sources allowed to connect (addresses or ranges such as `203.0.113.7` or `203.0.113.0/24`), and set the public address clients use. A small gateway container, built from the OpenRDS image, forwards only allowed sources; Docker publishes ports ahead of host firewalls such as `ufw`, so these rules are what protects the port. **Recent connection attempts** lists who tried, with one-click **Allow** for a blocked address. Rule changes apply at once. Turn on Require TLS before opening a database to the internet; the dashboard warns when a public database allows everyone or does not require TLS.

Databases created by earlier versions get TLS, password changes and database users after one reboot.

## Servers

OpenRDS runs databases on more than one machine. The console stays on its own server and manages every other server's Docker over SSH, so each database keeps everything it has on the console's server: backups and point-in-time recovery, upgrades, users, pooling, monitoring and alerts. If the console is down, databases keep running everywhere; only management and backups pause.

- **Add a server** from **Servers → Add server** (administrators). It needs Docker Engine 28 or newer and SSH reachable from the console. Run the command the dialog shows on the new server once: it creates an `openrds` user in the `docker` group that accepts the console's own SSH key. Then enter its SSH address (`openrds@10.0.0.5`, or `user@host:port`), **Check host key**, compare the fingerprint with `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the server, and confirm. OpenRDS connects only to that key. Being in the `docker` group is effectively root on that server, so keep the console's data volume private.
- OpenRDS checks the server's Docker and copies its helper images there: the gateway for public and private access, the connection pooler and the disk helper. They need the console's CPU architecture; on another one, those features are unavailable on that server. Database images are pulled by the server itself.
- **Choose the server** when you create or restore a database; the form starts with the reachable server that has the most memory not yet given to databases, and shows that server's capacity. A database stays on its server. To move one, restore a snapshot onto another server, or create a read replica there and promote it. Ports only need to be free on their own server, so the same port can be used on two servers. Read replicas can run on any server (see [Read replicas](#read-replicas)).
- **Private access** reaches a database from your other servers over a private network (a cloud VPC, WireGuard or Tailscale). Give the server its private IPv4 address on the **Servers** page, then turn on **Private access** under **Connect → Edit network**. A gateway on that address and the database's port forwards only the sources you allow; by default the private ranges `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` and `100.64.0.0/10`. The connection guide adds a **From the private network** string, and the database's certificate names the server's private and public addresses, so `verify-full` works over the private network too. Servers without a private address use public access instead.
- The **Servers** page shows each server's status, CPUs and memory, the memory given to its databases, whether it supports disk limits, and its databases; the **Databases** list shows each database's server and filters by it. A server's **public address** is the default public endpoint for its databases.
- **Losing a server.** If OpenRDS can't reach a server, its databases show **Server unreachable**, other servers keep working, and Events and notifications report it, and again when it is back. Capped databases a server's restart left stopped start again once their disk is attached.
- A server with databases can't be removed: delete them, or move them with a snapshot, first. Removing a server forgets its host key; remove the console's key from its `authorized_keys` too.

## Connection pooling

PostgreSQL gives every connection its own process, so an app with many workers, processes or serverless functions runs into `max_connections` and spends memory and time on connecting. **Connection pooling**, like RDS Proxy, puts PgBouncer in front of the database: many clients share a few server connections. Turn it on under **Connectivity & security → Connection pooling**; the database keeps running.

- **Transaction mode** (the default) lends a server connection for one transaction at a time, so a pool of 20 can serve hundreds of clients. Prepared statements work. Session state does not carry over between transactions: `SET` (use `SET LOCAL`), `LISTEN`, session advisory locks and temporary tables need **session mode** or a direct connection.
- **Session mode** keeps a server connection per client until it disconnects. Everything works; it saves only the cost of connecting.
- **Pool size** is the number of server connections per user and database (20 by default). The pooler opens at most `max_connections` less 10, so direct connections, backups and OpenRDS always fit. **Client connections** is how many clients it accepts at once (1,000 by default, up to 50,000).

The pooler listens on its own loopback port (6432, or the next free port) and as `openrds-<name>-pooler:6432` on the database's Docker network, and the connection guide adds a **Through the connection pooler** string. It accepts the owner and every database user with their passwords; a new user or password works through it at once, without disconnecting anyone. It uses TLS when the client asks, or always when the database requires TLS, and the database's certificate names the pooler, so `sslmode=verify-full` works. Changing pooling settings restarts only the pooler, so clients connected through it reconnect. Public access still reaches the database directly. In a test with `max_connections` 20, 60 `pgbench` clients failed directly with *too many clients already* and ran without errors through the pooler.

## Read replicas

A **read replica** is a read-only copy of a database that follows its changes, usually within a second. Send reports, dashboards and other reads to replicas to take load off the source. Create one from **Overview → Read replicas** or **Actions → Create read replica**; a database can have up to 5.

- A replica is a database of its own: its own port and Docker network name (`openrds-<replica>`), size, connection pooler, public access, backups and tags. It starts with its source's storage, parameters and TLS setting. It runs its source's engine version.
- **On another server.** With more than one server, choose where the replica runs. A replica on another server keeps a copy of the data if the source's server is lost; promote it to take over. It connects over the **private network** when both servers have a private address: the source's server listens on its private address and the source's port, and lets in only the replica's server. Otherwise it connects **over the internet** through the source's public access, which then also lets in the replica server's public address; turn on public access and Require TLS for the source first. Replication always checks the source's certificate, so a source started before its server had that address needs one reboot. While a replica uses a route, the source's port, public access and the servers' addresses it relies on can't change; promote or delete the replica first. Promoting or deleting it closes the source to its server again.
- It has the **same users and passwords** as its source. Create users and change passwords on the source; they reach its replicas at once.
- **PostgreSQL** copies the source with `pg_basebackup` and streams its WAL through a replication slot. The slot keeps WAL on the source while a replica is away, up to a quarter of the source's disk limit (16 GB without one); a replica that falls further behind stops and must be created again. A replica needs `max_connections` and `max_worker_processes` at least as high as its source's, so raise them on replicas first. **MySQL** loads a consistent dump of the source and applies its binary log from that point; the replica is read-only even for administrators. **Valkey** copies the data with its own replication, over TLS when the source requires it. Replication connections always use TLS for PostgreSQL and MySQL.
- The source keeps running while a replica is created. Other changes to the source wait until its data is copied.
- A replica's **Overview**, its source's **Read replicas** panel and the **Databases** list show each replica's state and lag, and **Monitoring** charts a PostgreSQL or MySQL replica's lag. Notifications report a replica that stops replicating, cannot reach its running source for 10 minutes, or falls 10 minutes behind, and when it recovers.
- **Promote** makes a replica a standalone database that accepts writes, keeping its endpoint, data, users and settings. The source is not changed. To replicate again, create a new replica.
- A database with replicas cannot be deleted or upgraded: promote or delete its replicas first. Point-in-time recovery stays on the source; a MySQL source closes its binary log every minute while it is on, and replicas follow without a pause.

## Modify, reboot and protect

For one change at a time, use the database's **Settings** tab: each setting (instance size, host port, storage, automated backups, Require TLS, deletion protection and engine parameters) has its own **Change** button or switch, and changes that need a restart ask whether to apply now or at the next restart.

**Modify** changes the CPU and memory limits, host port, backup retention and window, engine parameters, deletion protection, Require TLS and tags. It ends with a summary of modifications showing each attribute’s current and new value:

- Retention, window, deletion protection and tags apply at once, without a restart.
- CPU, memory, engine parameters and Require TLS need a new container. For a running database, choose **Apply during the next restart** (Reboot, or Stop and Start) or **Apply immediately**, which restarts it now. A pending change shows on the database page until it applies; a stopped database applies changes when it starts.
- A port change always applies immediately and restarts a running database, as in RDS.

If an immediate change cannot start, for example because the new port is used by another program on the VPS, OpenRDS restores the previous settings, restarts the database with them, and reports why.

A starting database gets up to 15 minutes to become healthy, enough to replay its write-ahead log after the host lost power. A database that exits while starting, for example over a setting it refuses, fails within seconds instead. Stopping or rebooting gives the database up to 2 minutes to finish writes and flush its caches, and Docker waits as long when the host shuts down, so a busy database is not killed mid-write. PostgreSQL gets a `/dev/shm` as large as its memory limit (1 GiB without one), which parallel queries on large tables need: Docker's 64 MB default makes them fail with `could not resize shared memory segment`. Databases created before these settings existed get them at their next start or reboot.

Turn on **deletion protection** to make delete fail until it is turned off. When you delete a database you can take a **final snapshot** first; it needs a running database, and if it fails nothing is deleted.

### Engine parameters

Each engine offers a checked list of settings, such as `shared_buffers`, `work_mem`, `max_connections` and `shared_preload_libraries` (`pg_stat_statements` by default, for the Performance tab) for PostgreSQL; `innodb_buffer_pool_size`, `slow_query_log` and `sql_mode` for MySQL; `maxmemory-policy`, `save` and `notify-keyspace-events` for Valkey. `GET /api/engines` and the parameter editor list every setting with its type, range, default and description. Values are validated before they reach the database command line, and a cache size (`shared_buffers`, `innodb_buffer_pool_size`) must be smaller than the memory limit.

For heavy workloads, PostgreSQL also offers autovacuum settings, `wal_compression`, `checkpoint_completion_target`, `synchronous_commit`, parallel worker limits, `hash_mem_multiplier`, `track_io_timing` and `idle_session_timeout`; MySQL offers `innodb_io_capacity_max`, `innodb_log_buffer_size`, `innodb_parallel_read_threads`, the per-query `sort_buffer_size` and `join_buffer_size`, and `max_execution_time`; Valkey offers `activedefrag` and `maxmemory-eviction-tenacity`.

**Memory settings follow the memory limit**, as RDS sizes them to the instance class, so a larger instance uses its memory without tuning:

| Engine | Setting | With a memory limit |
| --- | --- | --- |
| PostgreSQL | `shared_buffers` | 25% of the limit |
| PostgreSQL | `effective_cache_size` | 75% of the limit |
| PostgreSQL | `maintenance_work_mem` | 1/16 of the limit, from 64MB to 2GB |
| MySQL | `innodb_buffer_pool_size` and `innodb_buffer_pool_instances` | 75% of the limit less 256 MiB for the server, in one instance per GiB up to 8, rounded down to whole 128 MiB chunks per instance so MySQL never rounds it up |
| Valkey | `maxmemory` | 75% of the limit |

A value you set yourself always wins; if you set `innodb_buffer_pool_size`, MySQL chooses its own instance count. Without a memory limit the engine defaults stay, because OpenRDS cannot know how much of the host a database may use. **Settings** shows the sized values, and they change with the memory limit at the next restart.

Parameters are passed on the command line from a generated Compose override, so they apply when the container starts. **Settings** lists each one as applied or pending a restart. Valkey’s append-only persistence is not offered: when AOF is switched on through a restart, Valkey starts from an empty append-only file instead of its RDB snapshot. Restores keep the snapshot’s parameters unless you change them.

### Limit an instance’s disk usage

Set **Data volume limit (GiB)** when creating or restoring an instance. For example, `20` (API: `"diskGiB": 20`) gives that database a fixed-size ext4 filesystem. Docker has no portable volume-size setting, so OpenRDS stores a sparse disk image in the `openrds_disk_images` volume, attaches it with a loop device, and mounts it through the normal local volume driver. Writes past the filesystem fail. Usable space is slightly less than the requested size because ext4 keeps its own metadata. The image is a ceiling, not reserved free space. Docker logs, image layers and OpenRDS backups are outside the cap. **Settings → Storage → Measure usage** estimates allocated data-file space on demand.

This needs rootful Docker on Linux, because attaching the image uses a short-lived privileged helper (`openrds-disk-helper`). The database container itself stays unprivileged. Each capped database uses one loop device until it is deleted. After a server or Docker restart, OpenRDS reattaches those devices when it starts. Docker may try a capped database before that and give up when its disk is missing; OpenRDS then starts it and records **Start database after Docker restart** in Events. Databases you stopped yourself stay stopped. Rootless Docker and daemons that cannot create loop devices show **Limits not enforceable here** and reject a capped request before creating an instance. Leave the field blank (API: `diskGiB: 0`) for a regular volume.

Stop a capped database and choose **Settings → Storage → Resize** to increase or decrease its limit. Growing extends the ext4 filesystem. Shrinking is refused when the data does not fit, and the previous size is kept. The database stays stopped until you start it again. A limit cannot be added to or removed from an existing volume in place: restore a backup into a new instance for that change. Restores inherit the source cap unless you override it; backups from before this feature default to uncapped.

## Backups and snapshots

- **Automated backups** run during each database’s backup window (UTC, at least 30 minutes long) and are kept for its retention period of 1–35 days. They run once a day by default; choose a **frequency** of every 1, 2, 3, 4, 6 or 12 hours to lose less data if something goes wrong. More frequent backups repeat from the window’s start, each run open for the window’s length (at most the interval), so every 6 hours from 03:00 means 03:00, 09:00, 15:00 and 21:00 UTC. Each backup is a full logical copy, so the form shows how many a schedule keeps: every hour for 7 days keeps about 168. The disk reserve still stops a backup that would fill the disk. The scheduler checks every minute. A database that is stopped, busy or unhealthy for a whole window misses that run, and the dashboard recommends checking any database whose latest backup is more than its interval plus two hours old. Automated backups expire after their retention period, even when their database has been deleted. A new retention period applies to the database’s existing automated backups; turning automated backups off keeps the existing ones until they expire. The console suggests a free half-hour window for each new database so databases on one host do not all back up at once; an API request without `backupWindow` uses 03:00–03:30.
- **Snapshots** are manual: **Actions → Take snapshot** on a running database, or a final snapshot at deletion. They stay until you delete them. **Keep** turns an automated backup into a snapshot that never expires.

Backups cover one database: PostgreSQL uses a custom-format `pg_dump` of `app`; MySQL uses `mysqldump` of `app` with routines, events and triggers; Valkey writes an RDB snapshot. These are logical backups, not whole-server snapshots: additional databases, cluster roles and replication state are outside their scope; point-in-time recovery (below) adds the logs between them. MySQL’s consistent snapshot applies to transactional tables; avoid schema changes during a dump. **Restore** always creates a **new** database with fresh credentials and leaves the source and backup intact; the form starts from the source’s size, data volume limit and parameters. Valkey is stopped while the snapshot replaces its data files, then started so it loads that snapshot. If an import fails, the target is marked **Restore incomplete**.

MySQL dumps are plain SQL, so OpenRDS stores them gzip-compressed, usually several times smaller; PostgreSQL's custom format and Valkey's RDB files are compressed by the engine already. Backups taken before compression existed stay as they are and restore as before.

A snapshot, restore or import of a large database can take hours. It keeps running when you close the browser or the SSH tunnel drops, for up to 24 hours, and **Events** shows how it ended.

Backups are stored **on the same VPS**, usually on the disk the databases use, and a full disk stops every database on it. So a backup does not start unless the disk keeps a reserve free after it (5% of the disk, from 2 GiB to 10 GiB), judged by the size of the database's previous backup, and a backup that runs the free space down to the reserve stops and is removed. The dashboard warns when free space gets close. Connect **backup storage** (below), or download a copy to independent storage, to recover from losing the host. Downloaded `.dump`, `.sql.gz` (older MySQL backups: `.sql`) and `.rdb` files can also be restored with standard `pg_restore`, `gunzip` with `mysql`, and Valkey or Redis tools.

### Point-in-time recovery

Like RDS, a PostgreSQL or MySQL database can be restored to **any second** of its retention period, not only to its backups. Turn on **Settings → Point-in-time recovery**. It needs automated backups; PostgreSQL applies it with a restart, MySQL at once. Read replicas don't have it: turn it on for their source.

**PostgreSQL**

- PostgreSQL archives every finished WAL segment, and at least once a minute when anything changed, to an archive volume of its own. OpenRDS collects the segments every minute into the backup library, gzip-compressed, and copies them to backup storage when it is connected. So a restore can miss at most about the last two minutes.
- Automated backups become **physical base backups** (`pg_basebackup`), the starting points for replaying WAL. The first one is taken right after the restart. They also restore faster than logical dumps for large databases, into the same PostgreSQL version. Snapshots you take stay logical dumps, which restore into the same or a newer version.
- **Backups** shows the recovery window, from the end of the oldest base backup to the newest archived WAL, and **Restore to point in time** picks a second in it (in UTC). A **new** database is created from the newest base backup before that second, replays WAL up to it, and opens. The source keeps running. The new database gets the source's size, storage, parameters and TLS setting and its own owner password; database users from the source are turned off, so create them again.
- WAL older than the oldest base backup is removed, here and in backup storage, as backups expire. Turning point-in-time recovery off stops archiving; what was archived stays until the retention period ends. An engine upgrade starts a new window with a base backup of the new version, and keeps the old one for its retention period.
- After losing the server, connect the same backup storage on a new one, **Import** a base backup, and restore it with a target time (API: `targetTime`): the WAL comes from storage.

**MySQL**

- MySQL writes every change to its binary log anyway. Once a minute, when the open binary log holds changes, OpenRDS closes it (`FLUSH BINARY LOGS`) and copies the closed logs into the backup library, gzip-compressed, and to backup storage when it is connected. So a restore can miss at most about the last two minutes. The database keeps its own copies for `binlog_expire_logs_seconds`, which must be at least a day (or 0) while point-in-time recovery is on.
- Automated backups stay `mysqldump` dumps that record the binary log position they were taken at; **Backups** lists them as base backups. Recording the position takes a moment of global read lock, which waits for running queries, so a base backup doesn't start while a query has run for over 5 seconds, gives up after 5 more, and is tried again later. Snapshots you take are plain dumps.
- **Restore to point in time** creates a **new** database, loads the newest base backup before that second, then replays the binary logs from its position with MySQL's own replication applier, stopping before the first transaction committed after that second. Changes to database users and grants are skipped: the new database has its own owner password and none of the source's users, so create them again. Events wait while the logs replay, then run as on the source. A base backup restores into the same or a newer MySQL version.
- Binary logs older than the oldest base backup are removed from the library and backup storage as backups expire. If MySQL removed a log before OpenRDS copied it, for example while OpenRDS was down for longer than `binlog_expire_logs_seconds`, Events records it, and times between the backups around the gap can't be restored. An engine upgrade starts a new window with a base backup of the new data.
- After losing the server, connect the same backup storage on a new one, **Import** a base backup, and restore it with a target time (API: `targetTime`): the binary logs come from storage.

If collection fails five times in a row, Events records it, and notifications send it.

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

## Notifications

Admins choose where OpenRDS sends problems under **Administration → Notifications**, like RDS event subscriptions. A channel is a Slack incoming webhook, a Discord webhook, a Telegram bot in a chat, any webhook that accepts JSON, or email through your SMTP server. Each channel receives the kinds of events you pick:

| Events | When |
| --- | --- |
| **Database health** | A database turns unhealthy, crashes and is restarted by Docker (with a separate message when it ran out of memory), keeps crashing while starting, or stops on its own; and when it is healthy again. |
| **Failed operations** | Any operation in Events that failed, such as an automated backup, a restore or a copy to backup storage. The same failure is sent again only after 6 hours, so a copy retried every 10 minutes sends one message. |
| **Disk space** | Free space for backups falls below twice the reserve, falls below the reserve so backups pause, and recovers. |

OpenRDS checks every database once a minute. Changes an operation makes, such as a reboot or a stop you asked for, are not alerts, and the first check after the console starts only takes note. Health changes also appear in **Events**, where OpenRDS is shown as their author.

Adding a channel sends a test message first and saves the channel only when it arrives, so a mistyped URL is caught at once; **Send test** repeats it. Each channel shows when its last message went out, or why it failed. A message names the server, the time and a console link for the database, which opens through your SSH tunnel. Webhook receivers get JSON with `source`, `server`, `category`, `severity` (`critical`, `warning`, `resolved` or `info`), `database`, `title`, `detail`, `at` and the whole message as `text`.

For email, set up the server first: host, port, STARTTLS or TLS, an optional user and password (for Gmail, an app password) and the sender address. OpenRDS connects and signs in before saving, and never sends a password over an unencrypted connection except to a relay on the server itself. Webhook URLs, bot tokens and the SMTP password are kept in `notifications.json` in the control volume and are never shown again.

## Monitor and operate

| Console area | What you can do |
| --- | --- |
| **Getting started** | Follow a checklist that ticks itself off: create a database, connect an application, turn on automated backups, keep a copy off the VPS, and invite your team. It also explains the main ideas in plain language. The dashboard shows the next step until you finish or hide it. |
| **Dashboard** | On a new server, pick an engine to create your first database. Afterwards, see your databases with live CPU and memory, the next getting-started step, recommendations (unhealthy databases, backups off or missing, low disk space for backups, no notification channel, pending restarts, unprotected production databases, open or unencrypted public databases), resources with CPU and memory allocation, and recent activity. |
| **Databases** | Filter by name, port, tag, engine or status; sort by any column; select a database and use **Modify** or **Actions** (start, stop, reboot, take snapshot, delete). Each database page follows the RDS console: a summary, then tabs for connectivity and security, database users, monitoring, logs and events, configuration and parameters, maintenance and backups, and tags. |
| **Snapshots** and **Automated backups** | Restore, download, keep or delete recovery points. Automated backups shows how many databases back up automatically and one row per database with its schedule, last and next backup, and a **Turn on** or **Change** button that edits the schedule in place. |
| **Events** | Review the last 1,000 operations with who ran them and failure details, filtered by database or user, outcome or text. The sidebar counts new failures since your last visit. |
| **Users** and **Account** | Admins search and filter accounts, invite people, change a role straight from the list, resend pending invites, reset passwords, and disable or delete accounts. Everyone can rename themselves, change their password and sign out other sessions. |

The **Monitoring** tab charts CPU utilization (as a share of the CPU limit, or of the host when unlimited), memory in use against the limit, and network and disk throughput over the last 1, 3, 6, 12 or 24 hours. Every 30 seconds (`OPENRDS_METRICS_INTERVAL`, 5s–5m) the control plane reads each running database’s container statistics from the Docker Engine API through the mounted socket. The Docker CLI rounds statistics to three significant digits, which would make long-running throughput coarse. Readings are kept for 24 hours in memory and saved to `metrics.json` every five minutes and at shutdown, so an update keeps the charts. Each chart has a table view and a keyboard-readable crosshair.

Below those, three charts come from the engine itself, read every interval by running its own client inside the container: **Connections** (client connections, and for PostgreSQL and MySQL those running a statement), **throughput** (transactions per second for PostgreSQL, statements for MySQL, commands for Valkey) and the **cache hit ratio** (PostgreSQL's buffer cache, MySQL's InnoDB buffer pool, Valkey's keyspace hits). A falling hit ratio usually means the working set no longer fits in memory: raise the memory limit, and the memory settings follow it. Readings the engine does not answer, for example while it starts, show as gaps.

### Performance

The **Performance** tab shows what the database is doing, like RDS Performance Insights. It needs the Operator role, because statement text can contain data.

- **Running now** lists statements in progress, longest first, with their user and what they wait for (a lock, I/O, a sleep). **Cancel** stops one statement and rolls back its transaction; the session stays connected, and Events records who cancelled it.
- **Top statements** ranks normalized statements (values replaced by placeholders) by total time, with calls, mean time and rows, since the statistics were last reset. PostgreSQL reads them from `pg_stat_statements`, which OpenRDS loads by default and creates in the `app` database the first time you open the tab; databases created earlier load it at their next restart. MySQL reads `performance_schema`. **Reset statistics** starts them afresh, for example after adding an index.
- **Valkey** has no statements to rank, so the tab shows its **slow log**: commands slower than `slowlog-log-slower-than` (10 ms by default), newest first, with the client address.

OpenRDS's own queries, and the container's health check, are left out of all of these.

The sidebar groups the console into databases, backups, activity and administration, with a quick link and live status dot for each database. Collapse it to icons on a desktop; on a phone it opens as a drawer. The account menu at its foot holds account settings, the light, dark or system theme, and sign-out.

## Where state lives

- `openrds_control` is the Docker volume mounted at `/var/lib/openrds`. It holds the API key (`admin-token`), accounts (`users.json`, with password hashes), instance metadata and database passwords, backup storage settings (`offsite.json`), notification settings (`notifications.json`), the TLS certificate authority and database certificates (`tls/`), generated Compose overrides, activity history, metric history, and completed backups.
- Each database has its own named data volume. Its internal Compose project name is in `instances/<name>.json` in the control volume. Treat this metadata as secret.
- The control plane uses the host Docker socket to operate these projects and read their statistics, and Docker over SSH for other servers; anyone with shell access to the server, the API key or an administrator account is a trusted operator.
- Other servers are in `servers/`, and the console's SSH key, pinned host keys and SSH settings in `ssh/`. The key opens Docker on every server, so treat the control volume as secret.
- To update: run the install command again, or pull your chosen revision and run `sh install.sh`. Rebuilding the dashboard preserves existing database containers, volumes, and credentials. Existing databases keep automated backups off until you set a retention period. `docker compose down` in the checkout stops the dashboard; **`down -v` also deletes the control volume**.
- Sessions are signed with the API key. To rotate it and sign everyone out, run `docker compose stop openrds`, `docker compose run --rm --no-deps --entrypoint rm openrds /var/lib/openrds/admin-token`, then `docker compose up -d`.
