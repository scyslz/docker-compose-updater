# Compose Auto Updater (Go)

A small containerized Go utility with an embedded web UI. It discovers **currently-running** Docker Compose projects from Docker labels, then pulls and updates only the image-based services you select. It does not touch standalone `docker run` containers.

## Quick start

1. Start the container — running Compose projects are listed automatically (discovered from Docker labels, no scanning required).
2. Mount each host directory that contains a project you want to update at the **same absolute path** the Docker daemon reports (read-only), plus a writable dir to `/config`. Unmounted projects are still listed but marked "not mounted".
3. Open the web UI to tick the projects to update on schedule, or click **Check & update** to update one project immediately.

```yaml
ports:
  - "8080:8080"
environment:
  WEB_ADDR: ":8080"
  CONFIG_FILE: /config/updater.json
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - ./config:/config
  # Mount the dirs holding your compose projects at the SAME absolute path
  # the daemon reports (see `com.docker.compose.project.config_files`).
  - /home/hq/docker-data:/home/hq/docker-data:ro
```

Then run:

```sh
docker compose up -d
docker logs -f compose-auto-updater
```

Open `http://<host>:8080` for the web UI.

### Prebuilt image

The included GitHub Actions workflow (`.github/workflows/docker-publish.yml`) builds and pushes an image to GitHub Container Registry (`ghcr.io`) on every push to `main`/`master`, on version tags (`v*`), and on manual dispatch.

`compose.yaml` defaults to `ghcr.io/scyslz/docker-compose-updater:latest`. To use the prebuilt image instead of building locally, set the image without `--build`:

```sh
docker compose pull
docker compose up -d
```

Override the image (e.g. for a fork or a different registry) via an `.env` file next to `compose.yaml`:

```sh
COMPOSE_AUTO_UPDATER_IMAGE=ghcr.io/scyslz/docker-compose-updater:latest
```

## Environment variables

Only bootstrap settings are read from the environment; all updater settings live in the [configuration file](#configuration-file) and are edited through the web UI.

- `CONFIG_FILE`: path to the JSON configuration file; default `/config/updater.json`.
- `WEB_ADDR`: web UI listen address; default `:8080`. Set to `off` to disable the UI.
- `PREFER_COMPOSE`: force a Compose implementation. Unset (default) auto-detects `docker compose` (v2) first, then falls back to `docker-compose` (v1). Set to `v2` or `v1` to force one.

The following settings are configured in the file / web UI (not via environment), with these defaults: cron `0 */6 * * *`, timezone `Asia/Shanghai`, `run_on_start` and `auto_update` both `true`, command timeout `30m`, and no projects selected for scheduled updates.

## Web management UI

The updater embeds a small web UI (default `http://<host>:8080`) for viewing and editing configuration without touching files by hand:

- **Schedule**: edit `cron_schedule`, timezone, command timeout, and the `auto_update` / `run_on_start` toggles. Saving writes the config file and the scheduler picks up the new cron on its next cycle (no restart needed).
- **Running projects**: every currently-running Compose project is listed (from Docker labels). Tick a project to include it in the **scheduled** update; use **Check & update** to update just that project immediately — even if it is not ticked.
- **Scan all now**: run a full scan over all selected projects.
- Each project shows its last status: `updated`, `unchanged`, `skipped`, `unmounted`, `pulled`, or `failed`.
- Each project also shows **Last check** (when it was last inspected) and **Last update** (the last successful `updated`/`pulled`/`started` action). Last-update times are stored in the config file, so they survive restarts and self-updates.

Projects whose compose file is not mounted inside the container appear with a **not mounted** marker; their checkbox and button are disabled until you mount the directory.

The UI has **no authentication** — bind it to a trusted network or put it behind a reverse proxy. Set `WEB_ADDR=off` to disable it.

## Configuration file

Configuration is stored as a single JSON file (`CONFIG_FILE`, default `/config/updater.json`) so it survives restarts and can be managed from the web UI. Environment variables act as defaults and are used only **before** the file exists; once the file is present, its values win.

See `config.example.json` for the full shape:

```json
{
  "cron_schedule": "0 */6 * * *",
  "timezone": "Asia/Shanghai",
  "run_on_start": true,
  "auto_update": true,
  "command_timeout": "30m",
  "enabled_projects": {
    "/home/hq/docker-data/foo/docker-compose.yml": true
  }
}
```

`enabled_projects` maps an absolute compose file path (as reported by Docker) to `true` when it is selected for scheduled updates. Mount `./config` (or any host dir) to `/config` to persist it.

The updater also maintains a `last_updates` map (compose file path → RFC3339 timestamp) in the same file to power the **Last update** column. It is written automatically; you do not need to edit it.

### Check log retention

The web UI keeps the most recent **100** check entries in memory (`maxCheckLogs` in `web.go`). Once that limit is exceeded, the oldest entry is dropped, so the log rotates automatically rather than growing without bound. Each entry's captured command output is capped at 20 KB. The log is **not** written to disk and is cleared when the container restarts; use `docker logs` for the process log. Last-update timestamps are separate from this ring buffer and persist in the config file.

## Behavior and safety

- Discovers running projects from Docker container labels (`com.docker.compose.project*.config_files`); no directory scan is needed to list them.
- Only a project's **primary** compose file (first in `config_files`) is read; the running-project list still includes projects with multiple `-f` files.
- Requires the compose file to be readable inside the container (mount its directory at the same absolute path) before it can be updated.
- Only updates services that are currently running, have `image:`, and do not have `build:`.
- Auto-detects the Compose implementation: `docker compose` (v2) is preferred, falling back to `docker-compose` (v1). Force one with `PREFER_COMPOSE=v1|v2`.
- Scheduled scans process up to 4 projects concurrently (`maxParallelProjects` in `main.go`); Compose itself pulls a project's images in parallel. The updater's own project is always updated last so its container recreation does not interrupt other pulls.
- Records each running container's image ID, runs `compose pull <eligible services>`, then **only recreates services whose image ID actually changed**. Unchanged services are left untouched and the project is reported as "images unchanged, nothing to recreate".
- Recreates via `compose up -d --no-deps --no-build [--wait] <changed services>` when `AUTO_UPDATE=true`. `--wait` is added only on v2 (v1 has no `--wait`).
- Does not call `docker stop`, `docker rm`, `docker compose down`, or `--remove-orphans`.
- Does not update standalone containers just because they exist in Docker.

## Important limitations

This is a small starter utility, not a mature production updater. Test with non-critical stacks first. Compose files are treated as authoritative. If a project was originally started with multiple `-f` files, a custom `--project-directory`, profiles, or a non-default project name, this scanner may not reproduce the original invocation. Mount scanned directories at the same absolute paths inside the container to help preserve relative bind-mount and `.env` path resolution.

A failed update is logged but this version does not automatically roll back the image or application data. `--wait` relies on each service's configured healthcheck where applicable. The Docker socket grants broad control over the Docker host; do not expose a control API publicly.
