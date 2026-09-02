[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Installation

## Table of contents

- [Requirements](#requirements)
- [Install with Docker](#install-with-docker)
- [Install a prebuilt release](#install-a-prebuilt-release)
- [Build from source](#build-from-source)
- [Cross-compile (Windows)](#cross-compile-windows)
- [Next steps](#next-steps)

## Requirements

- The Go version declared by `go.mod`, currently `1.26.6` (only required if building from source)
- A Debian-based target host (Debian, Ubuntu, Proxmox, etc.) with `apt`
- `psmisc` on each target, which provides the required `/usr/bin/fuser` APT-lock check
- `sudo` for non-root target SSH users; root SSH sessions run maintenance commands directly
- `gpgv` and the distribution archive keyring for distribution-verified CVE coverage
- `systemd` and `systemctl reboot` when using the controlled reboot workflow
- Network access from the updater host to targets over SSH
- Outbound HTTPS from the updater process to `api.osv.dev` when CVE assessment is required, and to any configured notification destinations

## Install with Docker

Use the published image from GHCR (recommended):

```bash
cp .env-template .env
docker pull ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.7
docker run --env-file .env -p 8080:8080 -v debian-updater-data:/data ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.7
```

Open the UI:

- `http://localhost:8080`

Notes:

- The container stores the SQLite DB at `/data/servers.db` and the encryption key at `/data/config.json` when a volume is mounted.
- If you do not mount `/data`, state is not persisted.

Build locally (optional):

```bash
docker build -t debian-updater-web .
docker run --env-file .env -p 8080:8080 -v debian-updater-data:/data debian-updater-web
```

## Install a prebuilt release

Download the archive for your platform from [GitHub Releases](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/releases). Release archives include the binary, `templates/`, `static/`, documentation, and `.env-template`.

After extracting the archive, run `./webserver` from the extracted application directory. On Windows, run `webserver.exe`.

## Build from source

1. Build:

```bash
go build -o webserver .
```

2. Run:

```bash
./webserver
```

3. Ensure runtime assets are present:

- `templates/`
- `static/`
- Optional data directory (`./data/`) for persistence when not using `/data`

## Cross-compile (Windows)

From a Windows shell in the repo:

```bat
set GOOS=linux
set GOARCH=amd64
go build -o webserver .
```

Transfer `webserver` and the `templates/` and `static/` directories to the host you will run it from.

## Next steps

- Complete first-run setup at `/setup`, then sign in at `/login`
- Configure storage paths, metrics token, and backup/restore behavior: [configuration.md](configuration.md)
- Add servers and run updates: [usage.md](usage.md)
