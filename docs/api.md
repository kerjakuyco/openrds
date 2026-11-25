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
| GET / POST | `/instances` — each instance lists `tuned`, the memory settings sized to its memory limit |
| GET | `/engines` — engines, versions (`latest`, `stable`), the default version, suggested ports, memory minimums and adjustable parameters; `auto` on a parameter says how OpenRDS sizes it to the memory limit |
| GET | `/host` — Docker host name, CPU count, memory capacity, engine version, `volumeQuota.state` (`supported`, `unsupported` or `unknown`) and `backupDisk` (`freeBytes`, `totalBytes`, and `reserveBytes`, the free space backups leave) and `notificationChannels` |
| GET | `/instances/{name}/connection` (add `?user=` for a database user; each call is recorded), `/instances/{name}/metrics`, `/instances/{name}/logs`, `/instances/{name}/storage` (on-demand `usedBytes` for a running instance) |
| GET | `/instances/{name}/history?range=1h` — readings for `1h`, `3h`, `6h`, `12h` or `24h`, averaged to about 240 points |
| POST | `/instances/{name}/start`, `/instances/{name}/stop`, `/instances/{name}/reboot`, `/instances/{name}/backups` |
| POST | `/instances/{name}/modify` — any of `cpus`, `memoryMiB`, `port`, `backupRetentionDays`, `backupWindow`, `backupIntervalHours` (1, 2, 3, 4, 6, 12 or 24), `deletionProtection`, `tags`, `parameters`, `requireTLS`, `publicAccess`, `publicPort`, `publicHost`, `firewall` (`[{"cidr","description"}]`), plus `applyImmediately`; returns `{"restarted": …, "pendingRestart": …}`. Tags and parameters are replaced as a whole. |
| POST | `/instances/{name}/disk` — body: `{"diskGiB": 30}`; stopped capped instance only |
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
| GET | `/backups`, `/backups/{id}/download`, `/activity` |
| POST | `/backups/{id}/restore` — body: `{"name":"orders-restored","port":15433}`; engine, version, data-volume limit and parameters are inherited. Set `version` (same or newer), `diskGiB` or `parameters` to override them. |
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

Roles: viewer for every GET except `/connection` and `/download`, which need operator like all changes; admin for `/users` and `/notifications`. Backups report `kind` (`manual` or `automated`), `expiresAt` for automated backups, `final` for final snapshots, and `compression` (`gzip`) when OpenRDS compressed the dump, as it does for MySQL. `bytes` is the size stored and downloaded.

Operations are synchronous; the first provision may take several minutes to pull the image. A change keeps running for up to 24 hours even when the client disconnects, so a snapshot or restore of a large database finishes; `GET /activity` shows how it ended. Reads stop with their client after 30 minutes at most. Only one mutation runs per instance at a time. A failed creation keeps its record: fix the Docker error and choose **Start** to retry, or delete the instance to release its name and port.

A snapshot or import that would leave less than the reserve free on the backup disk (5% of it, from 2 GiB to 10 GiB) fails with `507`, as does one stopped because free space ran low while it was written.
