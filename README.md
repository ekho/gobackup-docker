# gobackup-docker

[![CI](https://github.com/ekho/gobackup-docker/actions/workflows/ci.yml/badge.svg)](https://github.com/ekho/gobackup-docker/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ekho/gobackup-docker?sort=semver)](https://github.com/ekho/gobackup-docker/releases)
[![Container image](https://img.shields.io/badge/ghcr.io-gobackup--docker-2496ED?logo=docker&logoColor=white)](https://github.com/ekho/gobackup-docker/pkgs/container/gobackup-docker)
[![Go Report Card](https://goreportcard.com/badge/github.com/ekho/gobackup-docker)](https://goreportcard.com/report/github.com/ekho/gobackup-docker)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

`gobackup-docker` configures [gobackup](https://github.com/gobackup/gobackup) from **Docker container labels**
instead of a hand-written `gobackup.yml`.

It runs as a small supervisor next to the stock `gobackup` container. It watches the Docker daemon, reads
`gobackup.*` labels off your containers, renders a complete `gobackup.yml`, and writes it to a volume shared with
gobackup — which hot-reloads it automatically. Add a labeled container and it starts getting backed up; remove it and
it stops. You never edit `gobackup.yml` by hand.

---

## How it works

```
                          reads gobackup.* labels
   ┌─────────────────┐    + a shared defaults.yml     ┌──────────────────────┐
   │  your containers │ ───────────────────────────▶ │  gobackup-docker      │
   │  (postgres, …)   │                               │  (this supervisor)    │
   └─────────────────┘                               └──────────┬────────────┘
            ▲  Docker events (start/die)                        │ renders one gobackup.yml,
            │  via /var/run/docker.sock:ro                      │ writes it atomically
            └───────────────────────────────────────           ▼
                                                     ┌──────────────────────┐
                                                     │  gobackup (stock)     │
                                                     │  fsnotify-reloads the │
                                                     │  file, runs on cron   │
                                                     └──────────────────────┘
                                                        shared config volume
```

The supervisor runs a single loop:

1. **Discover** — on startup it lists running containers, then subscribes to the Docker event stream and reacts to
   container `start` / `die` (reconnecting with backoff if the stream drops).
2. **Parse** — for each container it reads the flat `gobackup.*` labels into one backup *model*.
3. **Merge** — each model is deep-merged onto a shared *profile* from `defaults.yml` (so credentials, retention, and
   schedule are written once, not on every container), then per-model overrides and opt-outs are applied.
4. **Render** — it expands `{{ .Model }}`-style templates and marshals a single `gobackup.yml`.
5. **Apply** — it writes the file **atomically** (temp + rename) to the shared volume, de-duplicating so an unchanged
   result never touches the file. gobackup notices the change via its own file watcher and reloads — no restart.

A burst of events (e.g. `docker compose up`) is **debounced** into a single regeneration. Docker events only
regenerate the config; the actual backups run on gobackup's own schedule.

Backups themselves — database dumps, compression, encryption, upload, retention — are done entirely by gobackup.
This tool generates its configuration and, when a backup needs data that only lives in another container, **recreates
the gobackup container to mount that data in** (archive files read-only, SQLite database files read-write — see below).

---

## Quick start

`docker-compose.yml`:

```yaml
name: myproject

volumes:
  gobackup-config:   # shared: supervisor writes, gobackup reads
  gobackup-state:    # gobackup ~/.gobackup: retention state survives restarts

networks:
  backup_net:

services:
  gobackup-docker:
    image: ghcr.io/ekho/gobackup-docker:latest
    environment:
      GOBACKUP_DOCKER_OUTPUT: /etc/gobackup/gobackup.yml
      GOBACKUP_DOCKER_DEFAULTS: /etc/gobackup-docker/defaults.yml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./defaults.yml:/etc/gobackup-docker/defaults.yml:ro
      - gobackup-config:/etc/gobackup
    networks: [backup_net]

  gobackup:
    image: huacnlee/gobackup:latest
    command: ["/usr/local/bin/gobackup", "run", "-c", "/etc/gobackup/gobackup.yml"]
    environment:
      # ${VAR} in defaults.yml / labels is expanded HERE, by gobackup — so every
      # referenced secret must be present in THIS container's environment:
      YC_OS_ACCESS_KEY: ${YC_OS_ACCESS_KEY}
      YC_OS_SECRET_KEY: ${YC_OS_SECRET_KEY}
      TELEGRAM_BOT_TOKEN: ${TELEGRAM_BOT_TOKEN}
      NEXTCLOUD_DB_PASSWORD: ${NEXTCLOUD_DB_PASSWORD}
    volumes:
      - gobackup-config:/etc/gobackup
      - gobackup-state:/root/.gobackup
    networks: [backup_net]

  # An application you want backed up — it only needs its own database labels:
  nextcloud-db:
    image: postgres:16
    networks: [backup_net]
    environment:
      POSTGRES_PASSWORD: ${NEXTCLOUD_DB_PASSWORD}
      POSTGRES_DB: nextcloud
    labels:
      gobackup.enable: "true"
      gobackup.name: "nextcloud"
      gobackup.databases.db.type: "postgresql"
      gobackup.databases.db.host: "nextcloud-db"
      gobackup.databases.db.database: "nextcloud"
      gobackup.databases.db.username: "postgres"
      gobackup.databases.db.password: "$${NEXTCLOUD_DB_PASSWORD}"   # $$ keeps ${...} literal for gobackup
```

`defaults.yml` (shared building blocks, written once):

```yaml
default:                              # the profile every model inherits
  schedule:
    cron: "0 1 * * *"
  compress_with:
    type: tgz
  default_storage: s3
  storages:
    local:
      type: local
      keep: 10
      path: /backups/{{ .Model }}
    s3:
      type: s3
      bucket: media.kd
      region: ru-central1
      endpoint: https://storage.yandexcloud.net
      access_key_id: ${YC_OS_ACCESS_KEY}
      secret_access_key: ${YC_OS_SECRET_KEY}
      keep: 10
      path: /backups/{{ .Model }}
  notifiers:
    telegram:
      type: telegram
      chat_id: "66097481"
      token: ${TELEGRAM_BOT_TOKEN}
```

`docker compose up -d`, and the `nextcloud-db` container is backed up nightly to both S3 and local disk — with the
whole storage/notifier/schedule setup declared once in `defaults.yml`.

---

## Labels

One backup model per container. Everything is under the `gobackup.` namespace. Four keys are reserved meta; anything
else is a config path that goes straight into the model body.

| Label | Meaning |
|---|---|
| `gobackup.enable` | Opt-in gate. `"true"` to back up this container (leniently parsed: `true/1/yes/on`). |
| `gobackup.name` | Model name — the key under `models:` and the value of `{{ .Model }}`. Defaults to `<container>-<host>`. |
| `gobackup.instance` | Scope selector: only the supervisor with a matching `GOBACKUP_DOCKER_INSTANCE` manages this container. |
| `gobackup.profile` | Which profile in `defaults.yml` to inherit. Defaults to `default`. |
| `gobackup.<config.path>` | Anything else → the model body, e.g. `gobackup.databases.db.type`, `gobackup.archive.includes`, `gobackup.storages.s3.keep`, `gobackup.schedule.cron`. |
| `gobackup.<subtree>: "!none"` | Remove an inherited subtree, e.g. `gobackup.notifiers: "!none"` or `gobackup.storages.local: "!none"`. |

Because Docker Compose coerces unquoted `true`/`no`/`on` into booleans, **quote every label value**.

### Archive file backups with automatic volume mounting

`gobackup.archive.includes` backs up file **paths** — but those paths must exist inside the **gobackup container**, not
the application container. The supervisor can **automatically discover** which volumes the application container uses
and ensure they are mounted into the gobackup container.

**What happens**: when a model has `archive.includes`, the supervisor:

1. Inspects the source container's mounts via the Docker API.
2. Finds the MountPoint that matches each archive path.
3. Transforms the path → `/volumes/<model>/<original-path>` (so paths from different models never collide).
4. Stops, removes, and recreates the gobackup container with the discovered volumes mounted read-only (`:ro`).

**No manual volume pre‑mounting is needed** for named Docker volumes or bind mounts — the supervisor handles it.

```yaml
services:
  nextcloud:
    labels:
      gobackup.enable: "true"
      gobackup.archive.includes: "/var/www/html,/etc/nginx"
      gobackup.archive.excludes: "*.log,*.tmp"
  # The supervisor will:
  #   1. find nextcloud's volumes (e.g. nextcloud_html → /var/www/html)
  #   2. add them as read-only mounts on the gobackup container
  #   3. rewrite includes to /volumes/nextcloud/var/www/html
```

> **Excludes** are passed through as-is — they are glob patterns that apply inside the archive root, not paths that
> need mount resolution.

#### Controlling the recreated gobackup container (`gobackup_container.*`)

Recreating the gobackup container needs a spec (image, command, networks, …). By default the supervisor reuses the
settings of the container it replaces. To override them, put `gobackup_container.*` labels **on the supervisor's own
container** — they are read once at startup via the supervisor's self-inspection:

| Label (on the supervisor) | Example | Effect |
|---|---|---|
| `gobackup_container.image` | `huacnlee/gobackup:latest` | image of the recreated container |
| `gobackup_container.command` | `/usr/local/bin/gobackup run -c /etc/gobackup/gobackup.yml` | command — **must be the full argv** (the stock image has no `ENTRYPOINT`) |
| `gobackup_container.networks` | `backup_net,caddy_net` | networks to attach (real network names) |
| `gobackup_container.env.<VAR>` | `${DB_PASSWORD}` | one env var per label; when any is set it **replaces** the env set |
| `gobackup_container.labels.<key>` | `com.example.tier=infra` | extra labels passed through |

Rules:

- Each field **falls back** to the container being replaced when its label is absent — set only what you want to change.
- Base mounts (the config volume, `/backups`, state) are **preserved automatically**; only the auto-discovered
  backup-source volumes (archive includes and SQLite database files) are managed. Never list them here.
- The gobackup container **must already exist** and carry the label `gobackup-docker.component: "gobackup"` — that is
  how the supervisor finds the container to recreate. The supervisor recreates it; it does not create one from scratch.
- The supervisor needs the Docker socket mounted (`:ro` is sufficient — Docker API calls are socket messages, not
  file writes). gobackup itself never needs the socket.

```yaml
services:
  gobackup-docker:                         # the supervisor
    image: ghcr.io/ekho/gobackup-docker:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./defaults.yml:/etc/gobackup-docker/defaults.yml:ro
      - gobackup-config:/etc/gobackup
    labels:
      gobackup_container.image: "huacnlee/gobackup:latest"
      gobackup_container.command: "/usr/local/bin/gobackup run -c /etc/gobackup/gobackup.yml"
      gobackup_container.networks: "backup_net"

  gobackup:                                # recreated by the supervisor when auto-mounted volumes or credentials change
    image: huacnlee/gobackup:latest
    command: ["/usr/local/bin/gobackup", "run", "-c", "/etc/gobackup/gobackup.yml"]
    labels:
      gobackup-docker.component: "gobackup"   # ← how the supervisor finds it
    volumes:
      - gobackup-config:/etc/gobackup
      - backups:/backups
    networks: [backup_net]
```

#### Comma-separated arrays in labels

Some gobackup fields are YAML **arrays**, but a Docker label is a flat string. The supervisor converts
**comma-separated** values to proper arrays. Spaces around commas are trimmed; a single value becomes a one-element
array; an empty value becomes an empty array.

```yaml
labels:
  gobackup.archive.includes: "/var/www/html,/etc/nginx"   # → ["/var/www/html", "/etc/nginx"]
  gobackup.databases.pg.tables: "users,orders"            # → ["users", "orders"]
```

The full set of array fields (this is exhaustive for gobackup — every field it reads as an array):

| Label path | Applies to | gobackup meaning |
|---|---|---|
| `gobackup.archive.includes` | any model with an `archive` block | paths to back up |
| `gobackup.archive.excludes` | ″ | paths to skip |
| `gobackup.databases.<id>.tables` | mysql, postgresql | only these tables |
| `gobackup.databases.<id>.exclude_tables` | mysql, postgresql, mongodb | skip these tables/collections |
| `gobackup.databases.<id>.exclude_tables_prefix` | mongodb | skip collections with these prefixes |
| `gobackup.databases.<id>.skip_databases` | mssql (with `all_databases: "true"`) | databases to skip |
| `gobackup.databases.<id>.endpoints` | etcd | deprecated upstream — prefer the singular string `endpoint` |

> **Not arrays** (leave these as plain strings — do **not** comma-split them): `databases.<id>.args`, notifier
> recipients `notifiers.<id>.to` (gobackup splits these itself), and `storages.<id>.credentials` (a JSON blob that
> may contain commas). Only the paths in the table above are converted.

A value that must contain a literal comma can't be expressed this way — a rare case for table names and paths.

### SQLite databases by file path (automatic volume mounting)

gobackup dumps a SQLite database by reading its **file** (`sqlite3 <path> .dump`) — so, exactly like archive paths,
that file must exist inside the **gobackup container**, not the application container. Declare the database by the
path it has in your application and the supervisor mounts it into the engine for you:

```yaml
services:
  bot:
    volumes:
      - botdata:/app/data                                  # the app's own data volume
    labels:
      gobackup.enable: "true"
      gobackup.name: "shop"
      gobackup.databases.main.type: "sqlite"
      gobackup.databases.main.path: "/app/data/bot_database.sqlite3"
```

**What happens**: for each `type: sqlite` database with a `path`, the supervisor:

1. Inspects the source container and finds the volume (or bind mount) backing that path.
2. Rewrites `databases.<id>.path` → `/volumes/<model>/<original-path>` (so paths from different models never collide).
3. Recreates the gobackup container with that volume mounted — **read-write**.

The mount is **read-write**, unlike read-only archive mounts: `sqlite3 <path> .dump` opens the database, and a
WAL-mode database writes `-wal`/`-shm` sidecar files next to it on open. The volume's **directory** is mounted (not
just the file), so those sidecars have somewhere to go.

> Uses the same recreation machinery as archive auto-mount — the `gobackup_container.*` spec and the
> `gobackup-docker.component: "gobackup"` marker apply the same way (see above). A `path` already under `/volumes/`,
> or one not backed by any volume, is left untouched (with a log). The stock `gobackup` image already bundles
> `sqlite3`, so no extra setup is needed.

### Model naming

- With `gobackup.name` → used verbatim (e.g. `mynextcloud`).
- Without it → `<container-name>-<host>`, where `host` is the Docker daemon hostname (so backups from different hosts
  writing to the same storage don't collide). Override the host part with `GOBACKUP_DOCKER_HOST_ID`.

The model name also feeds `{{ .Model }}`, so storage paths like `path: /backups/{{ .Model }}` become
`/backups/nextcloud`, `/backups/gitea-prod-db-1`, etc.

Under Docker Compose the container name is `<project>-<service>-<n>` (e.g. `myproject-gitea-1`), which makes auto
names verbose. Set `gobackup.name` (or a fixed `container_name`) for tidy, stable model names.

### Templates vs. secrets — two substitution layers

- **`{{ .Model }}`** (also `{{ .Container }}`, `{{ .Host }}`, `{{ .Instance }}`) are Go-template tokens expanded by the
  **supervisor before the file is written**. Use them for names/paths.
- **`${VAR}`** (brace form) is left untouched by the supervisor and expanded by **gobackup at load time** (via
  `os.ExpandEnv` over the whole config file, plus an auto-loaded sibling `.env`). Use it for secrets — keep
  credentials in env, never baked into a label.

Two practical rules for `${VAR}`:

- The variable must exist in the **gobackup** container's environment (that is where expansion happens), not the
  application/database container's.
- In a **Compose** file, write `$${VAR}` or `$$VAR` in the label — Compose collapses `$$`→`$`, leaving `${VAR}`/`$VAR`
  for gobackup to expand (both the braced and bare forms are recognized as references). A single `$` would be
  interpolated (and likely blanked) by Compose itself.

Never write `${MODEL}` for the model name: gobackup would treat it as an environment variable and blank it. That is
why the supervisor uses `{{ ... }}`, and only for its own substitutions.

#### Literal `$` in values is escaped automatically

gobackup expands variables by running `os.ExpandEnv` over the **entire config file text** before parsing it. So **any**
`$` in **any** value — most often a password like `m9qq!$7v!s^$!UU` — would be eaten as a variable reference and
silently corrupted (→ `m9qq!v!s^UU`, an auth failure you'd chase for hours). YAML quoting can't prevent it: expansion
happens on the raw bytes before the parser sees the quotes, and `os.ExpandEnv` has **no escape character** (`$$`→``,
`\$`→`\`).

So the supervisor keeps a `$` only when it begins a real **variable reference** — `$NAME` or `${NAME}`, where `NAME` is
a letter followed by one or more letters/digits/underscores. Every other `$` is rewritten to `${GB_DOLLAR}`, a sentinel
variable the supervisor sets to a literal `$` in the engine's environment; gobackup then expands `${GB_DOLLAR}` back to
`$`, so the value round-trips exactly. Put a `$`-containing password like `m9qq!$7v!s^$!UU` straight in a label and it
just works — no escaping gymnastics for the value itself (Compose-level `$$` is a separate layer, applied to how Compose
reads your file).

Consequences:

- **Reference-shaped `$` still expands; everything else is kept literal.** `$DB_PASSWORD` and `${DB_PASSWORD}` are left
  as-is and expanded by gobackup; `$7`, `$!`, a trailing `$`, `$$`, and even a too-short name like `$A` or `${A}` are
  all preserved as a literal `$`.
- This needs the supervisor to **manage the engine** (Docker socket + `gobackup-docker.component=gobackup`), because it
  injects the `GB_DOLLAR` sentinel. In pure label-only mode it can't, so it instead **logs a warning** naming each
  value with a to-be-escaped `$` — resolve those with a `*_env`/`*_file` credential label (see below) or by writing the
  value through your own `${VAR}`.
- **A `$` that looks like a reference is treated as one.** Because `$word` / `${word}` are, by definition, the reference
  syntax, a secret that genuinely contains such a sequence (e.g. `p$word`, `Se${cret}`) is indistinguishable from a real
  variable — the supervisor leaves it alone and gobackup expands it (to empty if unset). Supply such values via a
  `*_env`/`*_file` credential label, which never round-trips through `os.ExpandEnv`.

### Credentials from an env var or a Docker secret

Instead of writing a credential inline (or wiring `${VAR}` yourself), a client container can **declare where the value
lives** and let the supervisor bridge it into the engine. Add an `_env` or `_file` suffix to a credential key:

```yaml
labels:
  gobackup.databases.main.password_env:  "DB_PASSWORD"          # value = $DB_PASSWORD in THIS container's env
  # or
  gobackup.databases.main.password_file: "/run/secrets/db_pw"   # value = a Docker secret file in THIS container
```

Supported credential keys: **`password`, `token`, `secret`, `access_key`, `secret_key`** (and the same under
`storages.<id>.*` / `notifiers.<id>.*`). `credentials_file` (gobackup's own GCS keyfile key) is **not** affected.
Set at most one of the inline key, `_env`, or `_file` — a conflict or an unresolved value **skips the model** (fail-closed).

Both render the config with a placeholder only — the plaintext is **never** written to `gobackup.yml`:

```yaml
databases:
  main:
    password: ${GB_<MODEL>_DATABASES_MAIN_PASSWORD}
```

How each is resolved (the engine container is recreated to apply it):

| Source | How the value reaches gobackup | Plaintext exposure | Rotation |
|---|---|---|---|
| `_env` | supervisor reads the var from the **client** container's env and sets it in the **engine's** env | visible in `docker inspect` of the engine (it was already an env var) | needs a recreate to change |
| `_file` | supervisor re-mounts the secret's host file into the engine (RO) and a command wrapper `cat`s it into the env at start | **none** — not in `gobackup.yml`, not in `docker inspect` (only the path shows); the supervisor never reads it | re-read on each config reload |

Notes & limits:

- `_file` works with **Compose `file:` secrets** (they have a host bind source). **Swarm** `docker secret` and Compose
  `environment:` secrets have no re-mountable host source → skipped with a log (use `_env`, or a bind-mounted file).
- The `_file` value is injected into the **daemon's** process env, so scheduled/API-triggered backups see it. A
  `gobackup perform` run via `docker exec` starts a fresh process and will **not** — trigger `_file`-credential backups
  through the schedule or the control-plane `POST /api/perform`, not `docker exec`.
- Requires the supervisor to manage the engine (Docker socket + `gobackup-docker.component=gobackup` on it), same as
  archive auto-mount.

### Overrides & opt-outs

A container inherits the profile and changes only what differs:

```yaml
labels:
  gobackup.enable: "true"                          # no gobackup.name → model "gitea-<host>"
  gobackup.databases.db.type: "postgresql"
  gobackup.databases.db.host: "gitea-db"
  gobackup.storages.s3.keep: "90"                  # override just this key (deep-merged over the profile)
  gobackup.notifiers: "!none"                       # drop the inherited notifiers for this model
```

---

## Shared defaults (`defaults.yml`)

The supervisor-only `defaults.yml` (path from `GOBACKUP_DOCKER_DEFAULTS`) holds named **profiles** — a "default model
body" every container inherits. gobackup never reads this file.

- You may use **YAML anchors and merge keys** (`&anchor`, `<<: *anchor`) inside it — they are resolved when the file
  is parsed and never leak into labels. Migrating an existing anchor-based `gobackup.yml` is close to copy-paste.
- Define more than one profile and select it per container with `gobackup.profile`.
- The supervisor also watches this file: edit it and the config is regenerated. A malformed edit is ignored and the
  last good config is kept.

**Merge precedence** (low → high):

1. gobackup's own built-in defaults;
2. the profile body from `defaults.yml` (`default`, or the one named by `gobackup.profile`);
3. the container's `gobackup.<config.path>` labels (deep-merged, per-key);
4. `"!none"` opt-outs (applied last, delete the named subtree);
5. `{{ ... }}` template expansion over the merged result (a rendering pass, not a value source).

---

## Worked example

Two applications, sharing the `defaults.yml` above:

```yaml
  nextcloud-db:
    labels:
      gobackup.enable: "true"
      gobackup.name: "nextcloud"                       # explicit name
      gobackup.databases.db.type: "postgresql"
      gobackup.databases.db.host: "nextcloud-db"
      gobackup.databases.db.database: "nextcloud"
      gobackup.databases.db.username: "postgres"
      gobackup.databases.db.password: "$${NEXTCLOUD_DB_PASSWORD}"

  gitea-db:
    container_name: gitea-db                           # fixed name → auto model "gitea-db-<host>"
    labels:
      gobackup.enable: "true"                          # no gobackup.name → "<container>-<host>"
      gobackup.databases.db.type: "postgresql"
      gobackup.databases.db.host: "gitea-db"
      gobackup.storages.s3.keep: "90"                  # keep 90 backups on S3 (not the default 10)
      gobackup.notifiers: "!none"                      # no notifications for this one
```

The supervisor renders (host = `prod-db-1`):

```yaml
models:
  gitea-db-prod-db-1:
    compress_with:
      type: tgz
    databases:
      db:
        host: gitea-db
        type: postgresql
    default_storage: s3
    schedule:
      cron: 0 1 * * *
    storages:
      local:
        keep: 10
        path: /backups/gitea-db-prod-db-1
        type: local
      s3:
        access_key_id: ${YC_OS_ACCESS_KEY}
        bucket: media.kd
        endpoint: https://storage.yandexcloud.net
        keep: 90
        path: /backups/gitea-db-prod-db-1
        region: ru-central1
        secret_access_key: ${YC_OS_SECRET_KEY}
        type: s3
  nextcloud:
    compress_with:
      type: tgz
    databases:
      db:
        database: nextcloud
        host: nextcloud-db
        password: ${NEXTCLOUD_DB_PASSWORD}
        type: postgresql
        username: postgres
    default_storage: s3
    notifiers:
      telegram:
        chat_id: "66097481"
        token: ${TELEGRAM_BOT_TOKEN}
        type: telegram
    schedule:
      cron: 0 1 * * *
    storages:
      local:
        keep: 10
        path: /backups/nextcloud
        type: local
      s3:
        access_key_id: ${YC_OS_ACCESS_KEY}
        bucket: media.kd
        endpoint: https://storage.yandexcloud.net
        keep: 10
        path: /backups/nextcloud
        region: ru-central1
        secret_access_key: ${YC_OS_SECRET_KEY}
        type: s3
```

Note how `gitea-db` dropped `notifiers` and bumped S3 `keep` to `90`, while `nextcloud` kept the full inherited setup —
and neither container repeated the S3 credentials, telegram token, or schedule.

---

## Supervisor configuration (environment)

| Variable | Default | Meaning |
|---|---|---|
| `GOBACKUP_DOCKER_OUTPUT` | `/etc/gobackup/gobackup.yml` | Where to write the generated config (a volume shared with gobackup). |
| `GOBACKUP_DOCKER_DEFAULTS` | `/etc/gobackup-docker/defaults.yml` | The shared profiles file. Missing file = label-only mode. |
| `GOBACKUP_DOCKER_INSTANCE` | *(empty)* | Only manage containers with a matching `gobackup.instance` (or none). Lets multiple supervisors coexist. |
| `GOBACKUP_DOCKER_HOST_ID` | *(daemon hostname)* | Host suffix for auto-generated model names. |
| `GOBACKUP_DOCKER_EXPOSED_BY_DEFAULT` | `false` | If `true`, back up every container unless it sets `gobackup.enable=false`. Default is strict opt-in. |
| `GOBACKUP_DOCKER_DEBOUNCE` | `500ms` | How long to coalesce a burst of Docker events before regenerating. |
| `GOBACKUP_DOCKER_HTTP_ADDR` | *(empty)* | If set (e.g. `:2705`), serve the control-plane API. |
| `GOBACKUP_DOCKER_GOBACKUP_URL` | *(empty)* | gobackup's web API base URL (e.g. `http://gobackup:2703`); enables the "backup now" proxy. |

The Docker connection is taken from the environment (`DOCKER_HOST`, TLS, etc.), so rootless and remote Docker work
without hardcoding the socket path.

---

## Control-plane API (optional)

Set `GOBACKUP_DOCKER_HTTP_ADDR` to expose the supervisor's own state and an on-demand trigger:

| Endpoint | Description |
|---|---|
| `GET /healthz` | Liveness check. |
| `GET /status` | JSON: instance, host id, discovered container/model count, last render time, last error. |
| `POST /api/perform?model=<name>` | Trigger a backup now — proxied to gobackup's `POST /api/perform`. Requires `GOBACKUP_DOCKER_GOBACKUP_URL`, and only forwards models the supervisor actually rendered. |

Example:

```bash
curl http://localhost:2705/status
curl -X POST "http://localhost:2705/api/perform?model=nextcloud"
```

gobackup's own API on `:2703` (`/metrics`, `/api/config`, `/api/log`) remains available directly. Keep both on an
internal network.

---

## Reaching databases and backing up files

- **Databases over the network (postgresql, mysql, mongodb, …):** the supervisor and the database share a Docker
  network, and the label `host` is the database's service name. gobackup connects over the network and runs the
  native dump tool (`pg_dump`, `mysqldump`, …), all of which are bundled in the stock `gobackup` image. These dumps
  need no access beyond the network. (File-based engines like SQLite are the exception — see the next bullet.)
- **File / volume backups (`gobackup.archive.includes`):** the supervisor **automatically discovers** the source
  container's Docker volumes and bind mounts, and ensures they are mounted into the gobackup container. Paths are
  rewritten to `/volumes/<model>/` to prevent collisions. No manual volume pre‑mounting is required — just add the
  labels. See [Archive file backups with automatic volume mounting](#archive-file-backups-with-automatic-volume-mounting)
  for details.
- **SQLite databases (`gobackup.databases.<id>.type: sqlite`):** file-based, not network. The supervisor mounts the
  DB file's backing volume into the engine **read-write** and rewrites `path` to `/volumes/<model>/…`, just like
  archive files. See [SQLite databases by file path](#sqlite-databases-by-file-path-automatic-volume-mounting).

---

## Security

Mounting `/var/run/docker.sock` — even read-only — grants broad access to the Docker daemon, and thus the host. Only
the small supervisor needs it (for discovery); gobackup does not. To harden, put a read-only
[docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) in front of it, restricted to container
list/inspect, and keep the whole stack on an internal network. Prefer the network+DSN path for databases so the
socket is used only for discovery.

## License

[MIT](LICENSE) © Boris Gorbylev. Use, modify, and distribute freely — the only condition is that the copyright
notice is retained in copies and substantial portions of the software (attribution may not be removed).
