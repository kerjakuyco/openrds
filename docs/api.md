# HTTP API

Everything the console does is available over HTTP, for scripts and automation. See the [user guide](guide.md) for what each feature does.

Scripts send the API key as `Authorization: Bearer <key>`; it has the Admin role. Print it on the server with `docker compose exec openrds openrds api-key`, and keep it secret. Browsers sign in with an account and get a 12-hour HTTP-only session cookie. Each endpoint needs at least the role in the table: viewer for reading, operator for changes, credentials and backup downloads, and admin for accounts. Inventory endpoints omit credentials; the connection endpoint returns them on request.

```sh
TOKEN=$(docker compose exec -T openrds openrds api-key)
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"orders","engine":"postgres","version":"18","port":15432,"cpus":1,"memoryMiB":1024,
       "backupRetentionDays":7,"backupWindow":"03:00-03:30","deletionProtection":true,
       "tags":{"env":"production"},"parameters":{"max_connections":"200"}}' \
  http://127.0.0.1:8080/api/instances
```

Only `name`, `engine` and `port` are required. An omitted `version` means the engine's stable version, omitted limits mean unlimited, an omitted `backupRetentionDays` means automated backups are off, and an omitted `backupIntervalHours` means daily.

| Method | Path (under `/api`) |
| --- | --- |
| GET / POST | `/instances` — each instance lists `tuned`, the memory settings sized to its memory limit, with pooling `pooler` and `poolerStatus`, with point-in-time recovery `recovery` (`earliest`, `latest`), a source's `replicas` (names), and a replica's `replicaOf` and `replication` as last read, plus its `server` (empty for the console's own), `serverName`, and with private access `privateHost`; `latestBackupAt`, `nextBackupAt` and `automatedBackups` (how many it keeps) describe its backups. Create on another server with `server`; `status` is `unreachable` while OpenRDS can't reach it |
| GET | `/servers` — every server with `reachable`, `problem`, `info` (`hostname`, `cpus`, `memoryBytes`, `dockerVersion`, `architecture`), `databases`, `allocatedMemoryMiB`, `volumeQuota`, `suggested` (where a new database goes by default) and its `privateAddress` and `publicAddress`; admins also see the SSH `address` |
| GET | `/servers/key` — admin: the console's SSH public key and the `setup` command for a new server |
| POST | `/servers/scan` — admin: `{"address"}` returns the server's `hostKeys` and their `fingerprints` |
| POST / PUT / DELETE | `/servers`, `/servers/{id}` — admin: add a server with `{"name","address","hostKeys","privateAddress","publicAddress"}` (the `hostKeys` you confirmed), change its name and addresses, or remove one that runs no databases |
| GET | `/engines` — engines, versions (`latest`, `stable`), the default version, suggested ports, memory minimums and adjustable parameters; `auto` on a parameter says how OpenRDS sizes it to the memory limit |
| GET | `/host` — Docker host name, CPU count, memory capacity, engine version, `volumeQuota.state` (`supported`, `unsupported` or `unknown`) and `backupDisk` (`freeBytes`, `totalBytes`, and `reserveBytes`, the free space backups leave) and `notificationChannels` |
| GET | `/instances/{name}/connection` (add `?user=` for a database user; each call is recorded; with private access also `privateHost` and `privateURI`), `/instances/{name}/metrics`, `/instances/{name}/logs`, `/instances/{name}/storage` (on-demand `usedBytes` for a running instance) |
| GET | `/instances/{name}/history?range=1h` — readings for `1h`, `3h`, `6h`, `12h` or `24h`, averaged to about 240 points; each carries `db` with `conns`, `active` (not for Valkey), `maxConns`, `ops` per second, `hit` (percent, when there were reads) and, for replicas, `lag` in seconds, when the engine answered |
| GET | `/instances/{name}/insights` — operator: `running` statements (`id`, `user`, `state`, `wait`, `seconds`, `query`), `statements` by total time (`query`, `calls`, `totalMs`, `meanMs`, `rows`; `null` with `statementsNote` when the engine does not collect them) and, for Valkey, the `slow` log |
| POST | `/instances/{name}/insights/reset` — operator: reset statement statistics or clear the slow log; `/instances/{name}/sessions/{id}/cancel` — cancel the statement a session runs |
| POST | `/instances/{name}/start`, `/instances/{name}/stop`, `/instances/{name}/reboot`, `/instances/{name}/backups` |
| POST | `/instances/{name}/modify` — any of `cpus`, `memoryMiB`, `port`, `pooler` (`{"enabled", "mode", "poolSize", "maxClients", "port"}`, PostgreSQL only: `mode` `transaction` or `session`; `{"enabled": false}` turns it off; omitted settings take their defaults and a changed one keeps the port), `backupRetentionDays`, `backupWindow`, `backupIntervalHours` (1, 2, 3, 4, 6, 12 or 24), `pitr` (point-in-time recovery for PostgreSQL and MySQL; PostgreSQL needs a restart, like `requireTLS`, MySQL applies it at once), `deletionProtection`, `tags`, `parameters`, `requireTLS`, `publicAccess`, `publicPort`, `publicHost`, `firewall` (`[{"cidr","description"}]`), `privateAccess` and `privateFirewall` (needs the server's private address; private ranges by default), plus `applyImmediately`; returns `{"restarted": …, "pendingRestart": …}`. Tags and parameters are replaced as a whole. |
| POST | `/instances/{name}/disk` — body: `{"diskGiB": 30}`; stopped capped instance only |
| POST | `/instances/{name}/replicas` — operator: `{"name","port"}`, optionally `cpus`, `memoryMiB` and `diskGiB` (by default the source's) and `server` (by default the source's), creates a read replica and returns `201 {"name"}` once it follows the source. A replica on another server has `sourceRoute` `private` or `public` |
| POST | `/instances/{name}/promote` — operator: make a replica a standalone database that accepts writes |
| POST | `/instances/{name}/move` — operator: `{"server","port"}` (`port` defaults to its own) moves the database to another server with seconds of write downtime, keeping its name and settings; returns `204` once it runs there |
| GET | `/instances/{name}/replication` — a replica's `state` (`replicating`, `syncing`, `connecting`, `broken`, `stopped` or `unknown`), `lagSeconds`, `detail` and `at`, read now |
| POST | `/instances/{name}/restore-to-time` — operator: `{"name","port","targetTime"}` creates a new database as the source was at `targetTime` (RFC 3339), within the source's `recovery` window |
| POST | `/instances/{name}/upgrade` — body: `{"version": "19"}`, a newer listed version: upgrades in place keeping the endpoint, after a snapshot (`note` says why it was taken); the previous version comes back if it fails |
| POST | `/instances/{name}/password` — change the owner password: `{"password": ""}` generates one; returns `{"password"}` |
| POST | `/instances/{name}/users` — `{"name","access","password"}` with `access` `read-only` or `read-write`; returns `{"name","password"}` |
| POST / DELETE | `/instances/{name}/users/{user}/password` (change), `/instances/{name}/users/{user}` (delete and end its sessions) |
| GET | `/instances/{name}/connections` — recent sources that tried the public port; `/tls/ca.pem` — the CA certificate |
| GET / PUT / DELETE | `/storage` — backup storage status (anyone), connect with `{"provider","endpoint","region","bucket","prefix","accessKeyId","secretAccessKey","pathStyle"}` after a test (admin; an empty secret keeps the saved one), or disconnect (admin) |
| POST | `/storage/test` — admin: test settings without saving |
| GET | `/storage/backups` — operator: copies in storage, with `local` when the library also has them |
| GET | `/notifications` — admin: channels with `target` (secrets left out), `categories`, `lastSentAt` and `lastError`, and the email server without its password |
| POST / PATCH / DELETE | `/notifications/channels`, `/notifications/channels/{id}` — admin: `{"kind","name","url","token","chatId","to","categories"}` with `kind` `slack`, `discord`, `telegram`, `webhook` or `email` and `categories` from `health`, `failures`, `disk`. A new or changed destination gets a test message before it is saved; an empty `url` or `token` keeps the saved one |
| POST | `/notifications/channels/{id}/test` — admin: send a test message |
| PUT / DELETE | `/notifications/smtp` — admin: `{"host","port","security","username","password","from"}` with `security` `starttls`, `tls` or `none`; checked by connecting and signing in. Removing it needs the email channels gone first |
| POST / DELETE | `/storage/backups/{id}/import` (bring a copy into the library as a snapshot), `/storage/backups/{id}` (delete a copy that is not in the library) |
| DELETE | `/instances/{name}` — add `?finalSnapshot=true` to take a final snapshot first; refused while deletion protection is on |
| GET | `/backups` — every backup, newest first. Filter with `name`, `kind` (`manual` or `automated`) and `since` (RFC 3339; created at or after). Add `limit` (0–500) and `offset` for a page, `{"items","total","bytes","offset","limit","names","kinds"}`: `total` and `bytes` cover every match, `names` lists the databases with backups of that kind, and `kinds` gives the `count` and `bytes` of `manual` and `automated` backups of that database. Sort a page with `sort` (`created`, `name`, `kind`, `size` or `expires`) and `order` (`desc`, the default, or `asc`) |
| GET | `/backups/{id}`, `/backups/{id}/download` |
| GET | `/activity` — the last 1,000 events, newest first. Filter with `name`, `result` (`started`, `completed` or `failed`), `q` (text in the operation, detail, name or person) and `since`. Add `limit` (0–500) and `offset` for a page, `{"items","total","count","offset","limit","names"}`: `total` counts every match, `count` every event, and `names` lists the databases and accounts in the history |
| POST | `/backups/{id}/restore` — body: `{"name":"orders-restored","port":15433}`; engine, version, data-volume limit and parameters are inherited. Set `version` (same or newer), `diskGiB` or `parameters` to override them. A physical backup (`format: "physical"`) restores into its own version, and with `targetTime` rolls forward to that time using WAL from the library or backup storage. A MySQL base backup (with `binlogFile`) restores into the same or a newer version, and with `targetTime` rolls forward by replaying binary logs. |
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

Roles: viewer for every GET except `/connection` and `/download`, which need operator like all changes; admin for `/users` and `/notifications`. Backups report `kind` (`manual` or `automated`), `expiresAt` for automated backups, `final` for final snapshots, and `compression` (`gzip`) when OpenRDS compressed the dump, as it does for MySQL. Base backups for point-in-time recovery have `startedAt` and `dataKey`: PostgreSQL's also `format: "physical"`, MySQL's the binary log position they were taken at, `binlogFile` and `binlogPos`. `bytes` is the size stored and downloaded.

Replies larger than about 1.4 KB are gzip-compressed for clients that send `Accept-Encoding: gzip` (`curl --compressed`).

Operations are synchronous; the first provision may take several minutes to pull the image. A change keeps running for up to 24 hours even when the client disconnects, so a snapshot or restore of a large database finishes; `GET /activity` shows how it ended. Reads stop with their client after 30 minutes at most. Only one mutation runs per instance at a time. A failed creation keeps its record: fix the Docker error and choose **Start** to retry, or delete the instance to release its name and port.

A snapshot or import that would leave less than the reserve free on the backup disk (5% of it, from 2 GiB to 10 GiB) fails with `507`, as does one stopped because free space ran low while it was written.
