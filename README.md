# Compose Auto Updater (Go)

A small containerized Go utility that scans configured directory trees for Docker Compose files, then pulls and updates only currently-running, image-based Compose services. It does not enumerate or update standalone `docker run` containers.

## Quick start

1. Mount each host directory you want scanned at the **same absolute path** inside the container (so Compose relative paths and `.env` files resolve as expected), and mount a host dir to `/config`.
2. Optionally seed the config by copying `config.example.json` to `./config/updater.json` and adjusting the paths. You can also start with an empty `./config` and configure everything in the web UI.
3. Start the container and open the web UI to set scan dirs, schedule, and per-project toggles.

```yaml
ports:
  - "8080:8080"
environment:
  WEB_ADDR: ":8080"
  CONFIG_FILE: /config/updater.json
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - ./config:/config
  - /data/docker:/data/docker:rw
  - /opt/stacks:/opt/stacks:rw
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

The following settings are configured in the file / web UI (not via environment), with these defaults: scan roots `/stacks`, depth `5`, cron `0 */6 * * *`, timezone `Asia/Shanghai`, `run_on_start` and `auto_update` both `true`, command timeout `30m`, and no exclusions.

## Web management UI

The updater embeds a small web UI (default `http://<host>:8080`) for viewing and editing configuration without touching files by hand:

- **Schedule & scanning**: edit `cron_schedule`, timezone, scan depth, include/exclude dirs, command timeout, and the `auto_update` / `run_on_start` toggles. Saving writes the config file and the scheduler picks up the new cron on its next cycle (no restart needed).
- **Projects**: every discovered Compose file is listed. Uncheck a project to exclude it from automatic updates. The per-file enable state is persisted in the config file.
- **Run now**: trigger a scan/update immediately.
- Each project shows its last-scan status: `updated`, `unchanged`, `skipped`, `pulled`, or `failed`.

The UI has **no authentication** — bind it to a trusted network or put it behind a reverse proxy. Set `WEB_ADDR=off` to disable it.

## Configuration file

Configuration is stored as a single JSON file (`CONFIG_FILE`, default `/config/updater.json`) so it survives restarts and can be managed from the web UI. Environment variables act as defaults and are used only **before** the file exists; once the file is present, its values win.

See `config.example.json` for the full shape:

```json
{
  "include_dirs": ["/data/docker", "/opt/stacks"],
  "exclude_dirs": ["/data/docker/compose-auto-updater"],
  "scan_depth": 5,
  "cron_schedule": "0 */6 * * *",
  "timezone": "Asia/Shanghai",
  "run_on_start": true,
  "auto_update": true,
  "command_timeout": "30m",
  "disabled_projects": {}
}
```

`disabled_projects` maps an absolute compose file path to `true` when it is excluded via the web UI. Mount `./config` (or any host dir) to `/config` to persist it.

## Behavior and safety

- Recognizes `compose.yml`, `compose.yaml`, `docker-compose.yml`, and `docker-compose.yaml`.
- Deduplicates Compose files when include roots overlap.
- Excluded directories are skipped recursively, including when an excluded directory is itself an include root.
- Skips projects with no running services.
- Only updates services that are currently running, have `image:`, and do not have `build:`.
- Auto-detects the Compose implementation: `docker compose` (v2) is preferred, falling back to `docker-compose` (v1). Force one with `PREFER_COMPOSE=v1|v2`.
- Records each running container's image ID, runs `compose pull <eligible services>`, then **only recreates services whose image ID actually changed**. Unchanged services are left untouched and the project is reported as "images unchanged, nothing to recreate".
- Recreates via `compose up -d --no-deps --no-build [--wait] <changed services>` when `AUTO_UPDATE=true`. `--wait` is added only on v2 (v1 has no `--wait`).
- Does not call `docker stop`, `docker rm`, `docker compose down`, or `--remove-orphans`.
- Does not update standalone containers just because they exist in Docker.

## Important limitations

This is a small starter utility, not a mature production updater. Test with non-critical stacks first. Compose files are treated as authoritative. If a project was originally started with multiple `-f` files, a custom `--project-directory`, profiles, or a non-default project name, this scanner may not reproduce the original invocation. Mount scanned directories at the same absolute paths inside the container to help preserve relative bind-mount and `.env` path resolution.

A failed update is logged but this version does not automatically roll back the image or application data. `--wait` relies on each service's configured healthcheck where applicable. The Docker socket grants broad control over the Docker host; do not expose a control API publicly.
