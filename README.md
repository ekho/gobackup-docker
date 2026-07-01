# gobackup-docker

A Traefik-style supervisor for [gobackup](https://github.com/gobackup/gobackup): it watches Docker containers, reads
`gobackup.*` labels, generates a gobackup config, and lets gobackup hot-reload it — so backups are configured by labels
instead of hand-editing `gobackup.yml`.

## Status

Design + Phase-0 spike complete; implementation starting.

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — research brief, gobackup facts, chosen architecture (Option B),
  and the label schema / DRY mechanism (§5).
- [docs/PLAN.md](docs/PLAN.md) — phased implementation plan.
- [spike/reload_test/RESULTS.md](spike/reload_test/RESULTS.md) — Phase-0 spike proving gobackup hot-reloads a
  config rewritten over a shared volume (→ no restart needed).

## How it works (in one line)

`docker events` → parse `gobackup.*` labels → deep-merge with a shared defaults profile → render one `gobackup.yml` →
**atomically write** it to a volume shared with the stock `gobackup` container, which reloads it via fsnotify.
