# Phase 0 spike — gobackup hot-reload over Docker volumes

**Date:** 2026-07-01 · **Runtime:** OrbStack (Docker 29.4, Linux VM, overlayfs) · **Image:** `huacnlee/gobackup:latest` = **gobackup 3.1.0**

## Question
Does gobackup reliably hot-reload its config when the file is rewritten by a **separate process/container** over a
shared mount — enough to make Option B (supervisor writes config, gobackup auto-reloads, no restart) viable?

## Method
- Named volume `gbcfg` shared by two containers: `gb` (gobackup running `run -c /gbcfg/gobackup.yml`, web API on :2703)
  and `gb-writer` (busybox, stand-in for our supervisor).
- Probe: `GET /api/config` returns the loaded model set (authoritative); logs print `[Scheduler] Reloading...` +
  `[Scheduler] Register <model>`. Each config version uses a **distinct model name** so a reload is unambiguous.

## Results

| Case | in-place write (`cat >`) | atomic rename (`mv` over) |
|---|---|---|
| **Named volume, cross-container** (representative of prod Linux) | ✅ instant | ✅ instant, **16/16 consecutive** renames |
| **Host bind-mount, edited from host** (OrbStack — macOS-advisory) | ✅ instant | ✅ instant |

Additional behaviors observed:
- **Broken/partial YAML** → container does **NOT crash** (`restarts=0`), but models drop to **`{}`** (gobackup does
  **NOT** keep last-good). Recovers cleanly when a valid file is written.
- **Valid YAML, invalid model** (no storage) → that model is **skipped with a log** (`load model X: no storage found`).
- **SIGHUP to `gobackup run`** → **terminates the process** (exit 2). SIGHUP-as-reload exists only in the daemonized
  `start` path, NOT in `run`. So reload = file change (fsnotify) only.

## Why rename is robust (not luck)
viper's `WatchConfig` watches the **directory** of the config file, so a `mv new → gobackup.yml` (inode swap) still
generates a CREATE event viper catches and re-reads. The watch survives inode changes indefinitely.

## Verdict → **Option B**
Apply = **atomic write to a shared volume; gobackup auto-reloads via fsnotify**. No process/container restart, no signal.
Even if later packaged as Option A (child process), the apply path is identical (SIGHUP would kill the child).

### Mandatory supervisor rules derived here
1. **Write atomically** (temp + `rename`) — a partial/broken file zeroes all models.
2. **Validate before writing** + keep supervisor-side **last-good**; never emit a config that yields zero/invalid models
   (fail-closed: skip a bad model with a log rather than writing it).
3. **Never use SIGHUP** to reload; rely on the file write.
4. **Prefer a named volume** over a host bind-mount for portability (bind-mount fsnotify worked on OrbStack but depends
   on the Docker host's file-sharing layer; not guaranteed on Docker Desktop's virtiofs/gRPC-FUSE).

## Reproduce
Fixtures: `alpha.yml`, `beta_inplace.yml`, `gamma_rename.yml`, `delta_sighup.yml` (each a valid 1-model config with a
distinct name). See this repo's git history / the commands in the Phase 0 session for the exact harness.
