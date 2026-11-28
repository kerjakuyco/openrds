# Contributing to OpenRDS

Thanks for helping make OpenRDS better! Bug reports, documentation fixes, clearer wording in the console and new features are all welcome, whatever your experience level.

Everyone taking part is expected to follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report a bug.** [Open a bug report](https://github.com/kerjakuyco/openrds/issues/new?template=bug_report.yml) with the steps to reproduce it, your OpenRDS commit, OS and Docker version.
- **Suggest a feature.** [Open a feature request](https://github.com/kerjakuyco/openrds/issues/new?template=feature_request.yml) that describes the problem you want solved.
- **Improve the docs.** Fix anything unclear in the [README](README.md) or the guides in [docs/](docs/).
- **Send a pull request.** Small fixes can go straight to a pull request. For a larger change, open an issue first so we can agree on the approach before you spend time on it.

Security problems are the exception: please don't report them in public. Follow [SECURITY.md](SECURITY.md) instead.

## Development setup

You need:

- **Go 1.26** or newer
- **Docker** with the **Compose plugin**: Docker Engine 28+ on Linux, or Docker Desktop
- **Python 3** and **Node.js** to run the end-to-end checks (optional for small changes)

Fork the repository, clone your fork, and run the console straight from source:

```sh
git clone https://github.com/<your-username>/openrds.git
cd openrds
go run .
```

The console opens at http://127.0.0.1:8080. Local state defaults to `.openrds/` and the listener to `127.0.0.1:8080`; override them with `OPENRDS_DATA_DIR` and `OPENRDS_LISTEN`. Database operations use the Docker CLI, daemon and Compose plugin. Metric history needs a unix Docker socket (`/var/run/docker.sock`, or `DOCKER_HOST=unix://…`); elsewhere the Monitoring tab shows live readings instead.

To run the packaged version as users do, run `sh install.sh` from your clone. It builds the image and starts it with Docker Compose.

The console is plain HTML, CSS and JavaScript modules embedded in the binary, so there is no frontend build step: restart `go run .` to see your changes.

## Project layout

| Path | What it does |
| --- | --- |
| `main.go`, `cli.go` | Startup, configuration and the `openrds` maintenance commands (`api-key`, `reset-password`, `gateway`) |
| `api.go`, `auth.go` | HTTP routes, roles and sessions |
| `serve.go` | Responses on the wire: compressed, revalidated console files and compressed JSON |
| `library.go` | The backup library and event history in memory, and pages of them with filters and sorting |
| `users.go`, `users_api.go` | Accounts, invitations and password hashing |
| `store.go`, `instances.go` | Database records, create, modify, start, stop and delete |
| `docker.go`, `engines/` | Engine definitions and the Compose templates each database runs from |
| `versions.go` | The engine version catalog: which versions you can choose, and which one is the default |
| `params.go` | Engine parameters and their validation |
| `access.go`, `tls.go`, `network.go` | Database users and passwords, TLS certificates, public access and the firewall gateway |
| `pooler.go`, `engines/pooler.yaml` | Connection pooling with PgBouncer for PostgreSQL |
| `upgrade.go`, `engines/upgrade.yaml` | Engine version upgrades that keep the endpoint |
| `pitr.go`, `engines/pitr.yaml` | Point-in-time recovery for PostgreSQL: WAL archiving and collection, base backups, restore to a time |
| `binlog.go` | Point-in-time recovery for MySQL: binary log collection, base backups with their position, replay to a time |
| `replicas.go`, `web/replicas.mjs` | Read replicas for every engine: creation, the source's network, replication status and lag alerts, promotion |
| `servers.go`, `engines/private-gateway.yaml`, `web/servers.mjs` | Servers: Docker over SSH with pinned host keys, helper images, reachability alerts, metrics over SSH, private access |
| `move.go` | Moving a database to another server: a replica there, a short write pause, promotion, and taking over the name |
| `scripts/test-server/` | A second server for the end-to-end checks: Docker-in-Docker with SSH |
| `backups.go`, `schedule.go`, `offsite.go`, `diskspace_*.go` | Snapshots, automated backups, copies to S3-compatible storage, and the free-space reserve backups keep |
| `quota.go`, `disk-helper.sh` | Data volume limits |
| `metrics.go`, `insights.go`, `activity.go` | Monitoring history, engine readings and Performance insights, and the event log |
| `notify.go` | Notifications: health and disk checks, and messages to Slack, Discord, Telegram, webhooks and email |
| `web/` | The console: HTML, CSS and JavaScript modules |
| `web/fonts/` | Source Sans 3 and IBM Plex Mono (Latin subsets), self-hosted, with their SIL Open Font License texts |
| `scripts/` | End-to-end checks against a real Docker installation |
| `docs/` | Install guide, user guide and API reference |

## Run the checks

Before you open a pull request, run:

```sh
go vet ./...
go test -race ./...
for f in web/*.mjs scripts/*.cjs; do node --check "$f"; done
```

For changes to listings, refreshes or anything else on every page load, compare the refresh benchmark before and after. It builds a library of 10,000 backups, so don't run it while the end-to-end checks below are running:

```sh
go test -run '^$' -bench Refresh -benchmem
```

For changes that touch Docker, engines or the console, also run the end-to-end checks against an installed copy, as CI does:

```sh
sh install.sh
python3 scripts/smoke.py
npm install --prefix /tmp/openrds-ui playwright-core
NODE_PATH=/tmp/openrds-ui/node_modules node scripts/ui-smoke.cjs
```

`scripts/smoke.py` exercises PostgreSQL, MySQL and Valkey against real Docker: accounts and roles; database users, password changes, required TLS and the firewall gateway; parameters, a deferred change applied by reboot, metric history, backups, deletion protection and a final snapshot restored into a new database; a snapshot of each engine's oldest version restored into its latest; MySQL point-in-time recovery; backup storage against a signature-checking S3 gateway, including an import and restore after losing the local copy; and a second server reached over SSH (a privileged Docker-in-Docker container), with a database there on its private address, a snapshot restored onto this server, a read replica on a second test server, a move between them, and a server going away and coming back.

`scripts/ui-smoke.cjs` drives Chrome on desktop and mobile: first-run setup, creating a database from the dashboard, every tab, settings changed in place, a snapshot, modify and reboot, backup storage, the restore form, events and themes, and a viewer who accepts an invitation in a second browser. On a fresh installation it creates the first administrator account; with accounts already there, it signs in through an invitation made with the API key. CI runs it after `scripts/smoke.py` on the same installation.

## Guidelines

- **Dependencies.** Keep the server on the Go standard library, and the console as plain HTML, CSS and JavaScript modules.
- **Content Security Policy.** The console runs under a strict policy: no inline scripts or styles and no `innerHTML`. Build elements with the `h()` helper in `web/lib.mjs`.
- **Fonts.** The console serves its own fonts, since the policy allows nothing from other origins. Use only fonts under a free licence such as the SIL Open Font License, and keep the licence text next to the files in `web/fonts/`.
- **Secrets.** Never return secrets in inventory responses, never log them, and keep passwords off command lines (send them on stdin).
- **Tests.** Add or update tests with your change. Go tests live next to the code (`*_test.go`); end-to-end behavior belongs in `scripts/`.
- **Wording.** Write user-facing text in short, plain sentences. Put longer explanations in a help toggle or the docs, not on the page.
- **Docs.** Update the [user guide](docs/guide.md) or [API reference](docs/api.md) when behavior changes.

### Adding an engine version

Add it to the catalog in `versions.go`, newest first, and move the `latest` or `stable` status if needed. Check that the image tag exists, then run `python3 scripts/smoke.py`: it restores a snapshot from each engine's oldest listed version into its latest. Update the version tables in the [README](README.md#supported-engines) and the [user guide](docs/guide.md#engine-versions).

## Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/) with a [gitmoji](https://gitmoji.dev/) prefix:

```text
✨ feat(ui): choose an engine version when creating a database
🐛 fix: start capped databases Docker could not start after a restart
📝 docs: step-by-step install guide
🔧 chore: add issue templates
```

Use `✨ feat` for features, `🐛 fix` for bug fixes, `📝 docs` for documentation, `♻️ refactor` for code changes that keep behavior, `✅ test` for tests and `🔧 chore` for everything else. Keep the subject short and in the imperative mood.

## Pull requests

1. Create a branch from `main` in your fork.
2. Make your change, with tests and docs.
3. Run the checks above.
4. Open a pull request and fill in the template. Include screenshots for console changes, in light and dark themes if colors changed.
5. CI must pass. A maintainer will review your pull request, and may ask for changes.

## License

OpenRDS is licensed under the [Apache License 2.0](LICENSE). By contributing, you agree that your contributions are licensed under the same license, as described in section 5 of the license.
