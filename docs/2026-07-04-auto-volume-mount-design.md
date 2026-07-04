# Auto Volume Mount — Design Spec

**Date:** 2026-07-04
**Status:** Implemented (with the as-built deviations below) · verified e2e in `spike/e2e-mount/`

## 0. As-built deviations from this draft

The feature shipped, but a few decisions changed during implementation — the sections below are the original draft;
where they differ, the as-built behaviour is:

- **Managed-container label** (§5.1): the label is **`gobackup-docker.component: "gobackup"`**, not
  `gobackup-docker.managed: "true"`.
- **Base mounts** (§3.2, §4.1, §6.2): preserved from the **gobackup container's own** mounts, not copied from the
  supervisor's. `mergeMounts` keeps every existing mount **not** under `/volumes/` (config, `/backups`, state) and adds
  the archive mounts. Copying the supervisor's mounts would miss `/backups` (the supervisor doesn't mount it). This
  also makes the mount set stable, so recreation is one-shot, not an endless loop.
- **No hardcoded defaults** (§3.3): there is no `ContainerDefaults`. Per field, `gobackup_container.*` wins; otherwise
  the value falls back to the **container being replaced**.
- **`gobackup_container.command` must be the full argv** (§3.2/§3.3): e.g.
  `/usr/local/bin/gobackup run -c /etc/gobackup/gobackup.yml` — the stock image has no `ENTRYPOINT`.
- **The gobackup container must pre-exist** (§2.1, §5.2): the supervisor **recreates** the container found by the
  component label; it does not create one from scratch when none exists (it logs and skips). Create it via compose.
- **Docker socket `:ro` is sufficient** (§9): container stop/remove/create/start work over a read-only socket mount —
  API calls are socket messages, not file writes. `rw` is not required. (Verified in the e2e.)
- Wiring: `internal/container.Parse` → `main.readSelfContainerConfig` (self-inspect via `os.Hostname`) →
  `Reconciler.WithGobackupSpec` → `pipeline.buildGobackupSpec`.

## 1. Problem

File backups (`gobackup.archive.includes`) require source volumes to be mounted into the gobackup container. Currently this must be done manually in `docker-compose.yml` — there is no dynamic discovery or mounting.

Docker cannot add mounts to a running container. The only way to change mounts is to stop, remove, recreate, and start the container.

## 2. Architecture (Approved)

### 2.1 Topology

Two containers, not one:

| Container | Role | Managed by |
|---|---|---|
| `gobackup-docker` | Supervisor: discovers containers, reads labels, manages lifecycle | docker-compose |
| `gobackup` | Backup engine: connects to DBs, runs tar/gzip/uploads | Supervisor via Docker API |

`gobackup` is NOT in `docker-compose.yml`. The supervisor creates and manages it through the Docker API.

### 2.2 Data Flow

```
                    ┌──────────────────────────────────────┐
                    │  docker-compose.yml                   │
                    │  services:                            │
                    │    gobackup-docker (supervisor)       │
                    │      labels: gobackup_container.*     │
                    │      volumes:                         │
                    │        /var/run/docker.sock:rw        │
                    │        gobackup-config:/etc/gobackup  │
                    │        ./defaults.yml:ro              │
                    └──────────┬───────────────────────────┘
                               │ ContainerInspect(self)
                               │ + detection of labeled containers
                               ▼
                    ┌──────────────────────────────────────┐
                    │  supervisor logic                     │
                    │                                       │
                    │  1. Inspect self → read gobackup_*    │
                    │     labels + volumes                  │
                    │  2. List containers with              │
                    │     gobackup.archive.includes         │
                    │  3. ContainerInspect → Mounts[]       │
                    │  4. Stop → rm → create → start       │
                    │     gobackup with archive volumes     │
                    └──────────────────────────────────────┘
                               │
                               ▼
                    ┌──────────────────────────────────────┐
                    │  gobackup (managed, not in compose)   │
                    │    image: huacnlee/gobackup:latest    │
                    │    env: [from gobackup_container.env] │
                    │    labels: [from gobackup_container.labels]│
                    │    volumes:                           │
                    │      gobackup-config:/etc/gobackup    │
                    │      gobackup-state:/root/.gobackup   │
                    │      nextcloud_html:/volumes/nextcloud/...:ro │
                    │    networks: [from gobackup_container.networks]│
                    └──────────────────────────────────────┘
```

## 3. Label Namespace (Approved)

### 3.1 Existing (`gobackup.*`, unchanged)

| Label | Purpose |
|---|---|
| `gobackup.enable` | Opt-in gate |
| `gobackup.name` | Model name |
| `gobackup.instance` | Scope selector |
| `gobackup.profile` | defaults.yml profile |
| `gobackup.<config.path>` | Model config body |

### 3.2 Container definition (`gobackup_container.*`, new)

| Label | Example | Purpose |
|---|---|---|
| `gobackup_container.image` | `huacnlee/gobackup:latest` | gobackup image (default in code, label overrides it) |
| `gobackup_container.command` | `run -c /etc/gobackup/gobackup.yml` | Command (default in code, label overrides it) |
| `gobackup_container.networks` | `backup_net,caddy_net` | Comma-separated network names |
| `gobackup_container.env.<VAR>` | `${NEXTCLOUD_DB_PASSWORD}` | One env variable per label |
| `gobackup_container.labels.<key>` | `caddy_0: gobackup.example.com` | Label passthrough to gobackup container |

**Do NOT configure in labels (auto-discovered from supervisor inspect):**
- Volumes (gobackup-config, gobackup-state) — copied from supervisor's own mounts

### 3.3 Defaults (hardcoded in code)

```go
type ContainerDefaults struct {
    Image   string   // "huacnlee/gobackup:latest"
    Command []string // ["/usr/local/bin/gobackup", "run", "-c", "/etc/gobackup/gobackup.yml"]
}
```

If the user specifies `gobackup_container.image` or `gobackup_container.command`, it overrides the default. Otherwise defaults are used.

### 3.4 Label passthrough

Everything under `gobackup_container.labels.*` is forwarded verbatim as a label on the gobackup container:

```
gobackup_container.labels.caddy_0                →  caddy_0: "gobackup.example.com"
gobackup_container.labels.caddy_0.reverse_proxy  →  caddy_0.reverse_proxy: "{{upstreams 2703}}"
gobackup_container.labels.caddy_0.tls            →  caddy_0.tls: "internal"
```

## 4. Volume Discovery (Approved)

### 4.1 Supervisor volume copy

On startup, supervisor inspects its own container (`ContainerInspect`) and reads `Mounts[]`. It copies every mount **except** `/var/run/docker.sock` (any mode) to the gobackup container.

This automatically brings in `gobackup-config`, `gobackup-state`, and any future shared volumes — with the same source (named volume or bind mount), the same target path, and read-only/writable mode preserved.

### 4.2 Archive volume discovery

For every discovered container with `gobackup.archive.includes` (or `gobackup.archive.excludes`):

1. Supervisor calls `ContainerInspect(id)` to read the source container's `Mounts[]`
2. For each path listed in `includes`/`excludes`, the supervisor finds the matching mount from `Mounts[]`
3. If no matching mount is found → log a warning and skip the path
4. The mount is added to gobackup's container definition

### 4.3 Mount target transformation

All archive volumes are mounted under `/volumes/<ModelName>/` preserving the relative path from the source container:

| Source mount | Mounted in gobackup at |
|---|---|
| `nextcloud_html → /var/www/html` | `nextcloud_html:/volumes/nextcloud/var/www/html:ro` |
| `/host/data → /data` | `/host/data:/volumes/nextcloud/data:ro` |

The rendered `gobackup.yml` uses the same prefix:

```yaml
models:
  nextcloud:
    archive:
      includes:
        - /volumes/nextcloud/var/www/html
        - /volumes/nextcloud/data
```

This prevents path collisions between models and keeps the mapping unambiguous.

### 4.4 Unmatched paths

If `archive.includes` references a path that has **no** corresponding mount in the source container's `Mounts[]`, a warning is logged and the path is **omitted** from the rendered config. No hard failure — the rest of the model is still generated.

## 5. Container Identity (Approved)

### 5.1 Managed container label

Every gobackup container created by the supervisor receives the label:

```
gobackup-docker.managed: "true"
```

### 5.2 Discovery on startup

1. Supervisor scans `ContainerList` for a container with `gobackup-docker.managed: "true"`
2. If exactly one is found → it is the managed gobackup container
3. If none is found → create a new gobackup container with `gobackup-docker.managed: "true"`
4. If multiple are found → disambiguate by matching `gobackup_container.image`; log ambiguity

### 5.3 Supervisor self-identification

To inspect itself, the supervisor reads its own container ID from the hostname (in Docker, the hostname defaults to the container ID).

```go
hostname, _ := os.Hostname()  // → container ID in Docker
```

If `os.Hostname()` returns an unexpected value, the supervisor falls back to `ContainerList` with label matching (it knows its own image and hostname).

### 5.4 Container name

The gobackup container is named `<supervisor-container-name>-gobackup` (e.g. `myproject-gobackup-docker-1-gobackup`).

## 6. Lifecycle (Approved)

### 6.1 Triggers

The supervisor regenerates the gobackup container when:
- Startup (initial create)
- A container start/die event changes the set of archive volumes
- A change in own labels is detected (via defaults.yml watcher or periodic)

### 6.2 Container recreation

When archive volume set changes:

1. `ContainerStop(gobackup-id)` — using Docker SDK
2. `ContainerRemove(gobackup-id)` — using Docker SDK
3. `ContainerCreate(...)` — using Docker SDK, with:
   - Base parameters from ContainerConfig (image, command, networks, env)
   - Labels from ContainerConfig.Labels + `gobackup-docker.managed: "true"`
   - Volumes from supervisor inspect (minus docker.sock) + archive volumes
4. `ContainerStart(gobackup-id)` — using Docker SDK

### 6.3 Config regeneration independent of container lifecycle

The supervisor continues to regenerate `gobackup.yml` on every event (debounced as before). Container recreation happens **in addition** to config writes, not instead of them.

## 7. Implementation Plan

### Phase 1: Docker client extensions

- Add `ContainerInspect(ctx, id)` method to `internal/docker/client.go` returning:
  - `Mounts[]`, `Labels`, `Config.Image`, `Config.Cmd`, `Config.Env`, `NetworkSettings.Networks`
- Add `ContainerCreate`, `ContainerStart`, `ContainerStop`, `ContainerRemove` methods

### Phase 2: Container config parser

- New package or file in `internal/labels/` (or new `internal/container/`) to parse `gobackup_container.*` labels
- Returns `ContainerConfig` struct: Image, Command, Networks, Env, Labels, Volumes

### Phase 3: Volume discovery logic

- In `internal/pipeline/reconcile.go` or a new package:
  - For each source with `archive.includes`, call `ContainerInspect` → `Mounts[]`
  - Match paths to mounts, build archive volume set
  - Transform paths to `/volumes/<ModelName>/`

### Phase 4: Container lifecycle integration

- After config is written, if archive volumes changed:
  - Stop → Remove → Create → Start gobackup container
  - Handle race conditions (supervisor restart mid-recreation)

### Phase 5: Supervisor self-inspect

- On startup, inspect own container for:
  - `gobackup_container.*` labels → build ContainerConfig
  - `Mounts[]` → copy to gobackup (minus docker.sock)

## 8. Error handling

### 8.1 Container recreation failure

If `ContainerStop`, `ContainerRemove`, `ContainerCreate`, or `ContainerStart` fails at any point:

1. Log the error with full context (which step failed, container ID, error details).
2. **Do not retry immediately** — the next debounced event will trigger a new attempt.
3. The old gobackup container continues running with its previous volume set. Backups for previously configured models continue unaffected.
4. The `status` API endpoint exposes the last lifecycle error.

### 8.2 Supervisor restart mid-recreation

If the supervisor is killed or restarted while recreating the gobackup container:

- If **after remove, before create**: no gobackup container exists. Supervisor on restart sees no `gobackup-docker.managed: "true"` container and creates a fresh one.
- If **after create, before start**: a stopped container with `gobackup-docker.managed: "true"` exists. Supervisor on start sees it and starts it.
- Idempotent by design.

### 8.3 Source container disappears

If a source container dies after volume discovery but before gobackup recreation, the `Mounts[]` data from the earlier `ContainerInspect` is still used. The mount will be stale (pointing to a removed named volume or bind path). gobackup will fail to archive that path → log error on the next backup run. On the next container list refresh, the source model is dropped from the config entirely.

## 9. Security

- Docker socket: currently `ro` → needs **`rw`** for container lifecycle operations
- All archive volumes are mounted **`ro`** in the gobackup container
- The gobackup container does NOT have docker.sock access

## 9. Limitations

- **No hot-add:** gobackup is stopped briefly during recreation (~1-2 seconds). Missed cron ticks are caught on the next schedule.
- **Docker compose collision:** if a user adds `gobackup` as a compose service alongside the supervisor, both will attempt to manage the container. Documented constraint: gobackup is managed by the supervisor, not compose.
- **Bind mounts from host:** supported, but the host path must exist on the same machine (trivial — the supervisor runs on the same host).
- **Tmpfs mounts:** not supported for archive volumes (tmpfs is per-container).
