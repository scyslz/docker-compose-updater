# Compose Auto Updater (Go)

A small containerized Go utility that scans configured directory trees for Docker Compose files, then pulls and updates only currently-running, image-based Compose services. It does not enumerate or update standalone `docker run` containers.

## Quick start

Edit `compose.yaml` and set `INCLUDE_DIRS` and `EXCLUDE_DIRS`. Paths are paths **inside the updater container**. Mount each included host directory at the same absolute path inside the container so Compose relative paths and `.env` files resolve as expected.

```yaml
environment:
  TZ: Asia/Shanghai
  INCLUDE_DIRS: /data/docker,/opt/stacks
  EXCLUDE_DIRS: /data/docker/archive,/opt/stacks/manual
  SCAN_DEPTH: "5"
  CRON_SCHEDULE: "0 */6 * * *"
  RUN_ON_START: "true"
  AUTO_UPDATE: "true"
volumes:
  - /var/run/docker.sock:/var/run/docker.sock
  - /data/docker:/data/docker:rw
  - /opt/stacks:/opt/stacks:rw
```

Then run:

```sh
docker compose up -d
docker logs -f compose-auto-updater
```

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

- `INCLUDE_DIRS`: comma- or semicolon-separated scan roots inside the container. Default `/stacks` if not set. Every root must be mounted into the container. `SCAN_PATHS` is supported as a backward-compatible alias when `INCLUDE_DIRS` is unset.
- `EXCLUDE_DIRS`: comma- or semicolon-separated directory paths to skip. Exclusions apply to the directory itself and all descendants. Paths are inside the container.
- `SCAN_DEPTH`: maximum directory depth below each include root; default `5`. Set `0` to inspect only files directly in each root.
- `CRON_SCHEDULE`: standard five-field Unix cron expression (`minute hour day-of-month month day-of-week`); default `0 */6 * * *` (every 6 hours at minute 0, in `TZ`). Supports lists, ranges, steps, and month/day names, e.g. `*/15 * * * *`, `0 3 * * 1-5`, or `0 */6 * * *`.
- `TZ`: timezone used for scheduling; default `Asia/Shanghai`.
- `RUN_ON_START`: run a scan immediately after startup; default `true`.
- `AUTO_UPDATE`: `true` pulls and recreates eligible services; `false` only pulls. Default `true`.
- `COMMAND_TIMEOUT`: timeout per Docker command; default `30m`.
- `PREFER_COMPOSE`: force a Compose implementation. Unset (default) auto-detects `docker compose` (v2) first, then falls back to `docker-compose` (v1). Set to `v2` or `v1` to force one.

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
