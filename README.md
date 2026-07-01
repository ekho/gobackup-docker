# gobackup-docker

A Traefik-style supervisor for [gobackup](https://github.com/gobackup/gobackup): it watches Docker containers, reads
`gobackup.*` labels, generates a gobackup config, and lets gobackup hot-reload it — so backups are configured by labels
instead of hand-editing `gobackup.yml`.

## Status

Working, tested (unit + two end-to-end), CI-gated. Pushing a `v*` tag publishes
`ghcr.io/ekho/gobackup-docker:<tag>` and `:latest` (see `.github/workflows/release.yml`).

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — research brief, gobackup facts, chosen architecture (Option B),
  and the label schema / DRY mechanism (§5).
- [docs/PLAN.md](docs/PLAN.md) — phased implementation plan.
- [spike/reload_test/RESULTS.md](spike/reload_test/RESULTS.md) — Phase-0 spike proving gobackup hot-reloads a
  config rewritten over a shared volume (→ no restart needed).

## How it works (in one line)

`docker events` → parse `gobackup.*` labels → deep-merge with a shared defaults profile → render one `gobackup.yml` →
**atomically write** it to a volume shared with the stock `gobackup` container, which reloads it via fsnotify.

## Control-plane API (optional)

Set `GOBACKUP_DOCKER_HTTP_ADDR` (e.g. `:2705`) to expose the supervisor's own state and a "backup now" action:

- `GET /healthz` — liveness.
- `GET /status` — JSON: instance, host id, discovered container/model counts, last render time, last error.
- `POST /api/perform?model=<name>` — trigger a backup now, proxied to gobackup's `POST /api/perform`. Requires
  `GOBACKUP_DOCKER_GOBACKUP_URL` (e.g. `http://gobackup:2703`) and only forwards models the supervisor rendered.

gobackup's own `:2703` API (`/metrics`, `/api/config`, `/api/log`) remains available directly. Keep both on an internal network.
