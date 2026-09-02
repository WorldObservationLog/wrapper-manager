# wrapper-manager

An HTTP management/proxy gateway that runs and supervises **multiple
[wrapper-lite](https://github.com/WorldObservationLog/wrapper) account
instances** (branch `lite`) behind one HTTP endpoint.

Each logged-in account is a long-lived `wrapper-lite-rootless` child process
(service mode) listening on a private random port. wrapper-manager starts,
logs in, logs out, restarts and restores those instances, and exposes a
wrapper-lite-compatible HTTP API that aggregates them: a request carrying an
`adamId` is forwarded to an instance whose Apple storefront region can serve
that item, and the wrapper-lite response envelope is passed through unchanged.

> **Not a decryptor.** This project no longer provides sample decryption and no
> longer speaks gRPC. Decryption is performed locally by
> [AppleMusicDecrypt](https://github.com/WorldObservationLog/AppleMusicDecrypt)
> v3 (Temari); wrapper-manager only supplies the `/key` `ctx`/`state` template
> and `/m3u8`/`/license` upstreams that the decryptor consumes over HTTP.

## Features

- **Multi-account management** — every account = one instance; requests are
  routed to the instance whose storefront region can serve the requested
  `adamId` (region availability is probed against the Apple catalog).
- **wrapper-lite-compatible HTTP API** — `/m3u8`, `/key`, `/lyrics`,
  `/webplayback`, `/license`, plus an aggregated `/status`.
- **`POST /login` / `POST /logout`** management endpoints, including a
  **two-phase 2FA login** flow.
- **Crash auto-restart** — when a service process exits it is relaunched and
  awaited until `/status` reports a region again (unless the account was
  logged out).
- **Startup recovery** — persisted accounts in `data/instances.json` are
  relaunched automatically (service mode only, no re-login).
- **First-run self-provisioning** — downloads the native wrapper-lite payload
  from nightly.link automatically when `data/wrapper/` is missing.

## Requirements

- Linux **x86_64** or **arm64**.
- `wrapper-lite-rootless` needs a **user namespace** (`unshare`) and
  **chroot**. Running inside a container therefore requires the container to
  run with `--privileged` (or with a seccomp/apparmor profile that permits
  user namespaces); see [Deploy](#deploy-docker-compose).
- Internet access to `nightly.link` on first run (payload download) and to
  Apple services (`amp-api.music.apple.com`, etc.) for login, region probing
  and upstream requests. A configured `-proxy` applies to all of them.

## Build & run

```shell
go build -o wrapper-manager .
./wrapper-manager
```

On first start the manager downloads and unpacks the wrapper-lite native
package for your architecture from
`https://nightly.link/WorldObservationLog/wrapper/workflows/build-lite/lite/wrapper-lite-linux-x86_64.zip`
(amd64) or `.../wrapper-lite-linux-aarch64.zip` (arm64). It is stored under
`data/wrapper/`. Use `-prepare` to perform only this download and exit
(handy for pre-seeding images).

### Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-host` | `localhost` | Host the HTTP server binds to (use `0.0.0.0` in containers). |
| `-port` | `8080` | Port of the HTTP server. |
| `-mirror` | `false` | Download wrapper-lite through `gh-proxy.com` instead of nightly.link (for users in China). |
| `-debug` | `false` | Enable debug output. |
| `-proxy` | *(empty)* | HTTP(S) proxy used by the manager for downloads/Apple probes and passed through to wrapper-lite (`--proxy`). |
| `-prepare` | `false` | Only download the required wrapper-lite payload, then exit. |
| `-device-info` | `Music/5.0.2/Android/10/Pixel 8/7663314/en-US/en-US` | Optional Apple Music device-info string (`--device-info` pass-through to wrapper-lite). |

## HTTP API

All responses use the wrapper-lite envelope, always `Content-Type:
application/json`:

```json
{ "code": 0, "msg": "SUCCESS", "data": { ... } }
```

| `code` | Meaning |
|---|---|
| `0` | Success. |
| `2` | 2FA code required — used by `POST /login` only (`data.loginId` tells the client which pending login to complete). |
| `-1` | Failure; `msg` carries the reason (manager-generated errors use `data: null`). |

### Resource endpoints (wrapper-lite compatible, multi-account aggregated)

The manager picks an instance whose storefront region can serve the request,
forwards to it, and passes the lite response body (envelope) through
untouched. No instance can serve the item → `code:-1, msg:"no available
instance"` (retry later).

| Method | Path | Parameters | Notes |
|---|---|---|---|
| `GET` | `/m3u8` | `adamId` | Playlist for the item. |
| `GET` | `/key` | `adamId`, `uri?` | Key/decryption context — `data` carries the `ctx`/`state` template used by AppleMusicDecrypt v3 + local Temari. |
| `GET` | `/lyrics` | `adamId`, `language?`, `syllable?` | Prefers an instance that actually has lyrics for the language (probed), falls back to region selection. |
| `GET` | `/webplayback` | `adamId` | Web-playback session data. |
| `POST` | `/license` | JSON body `{"adamId","challenge","uri"}` | License request forwarded to the selected instance. |
| `GET` | `/status` | — | Aggregated status: `data = {"status","regions","clientCount","ready"}`. |

`/status` fields:

- `status` — whether at least one instance is currently registered/running.
- `regions` — the deduplicated storefront regions of all running instances.
- `clientCount` — number of running (logged-in) instances.
- `ready` — whether the manager has finished restoring the accounts persisted
  in `instances.json` (`true` immediately when there are none).

### Management endpoints

| Method | Path | Body | Behavior |
|---|---|---|---|
| `POST` | `/login` | `{"username","password"}` or `{"username","password","code"}` | Two-phase login, see below. |
| `POST` | `/logout` | `{"username"}` | Kill the account's service process, delete its data dir, remove it from `instances.json`, return `code:0`. Unknown account → `code:-1, "no such account"`. |

**Two-phase 2FA login:**

1. `POST /login` with `{username, password}` starts the one-shot
   `wrapper-lite-rootless --login` child.
   - No 2FA needed: the request returns when the login settles →
     `code:0`, `data: {"loginId": "<id>"}` (tokens cached, service instance
     started and ready).
   - 2FA needed: returns as soon as lite prompts →
     `code:2, msg:"2fa code require"`, `data: {"loginId": "<id>"}`. The child
     keeps running and polls `<instance dir>/2fa.txt` for the code.
2. Re-`POST /login` with `{username, password, code}`. The manager writes the
   code to `2fa.txt`; this request blocks until the login completes and the
   service instance is ready → `code:0`, or `code:-1` on failure.

Notes:

- `/login` is idempotent per account: an account that is already logged in
  (registered with a region) returns `code:-1, "already login"`.
- Sending a `code` with no pending login returns `code:-1` with a message
  telling you to restart the login without a code.
- Failed logins clean up the instance data directory before reporting.
- The instance id is a deterministic UUIDv5 derived from the username, so the
  `loginId` is stable across attempts.

### Examples

```shell
# Aggregated status
curl -s http://localhost:8080/status
# {"code":0,"msg":"SUCCESS","data":{"status":false,"regions":[],"clientCount":0,"ready":true}}

# Start a login (this account requires 2FA)
curl -s -X POST http://localhost:8080/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"you@example.com","password":"hunter2"}'
# {"code":2,"msg":"2fa code require","data":{"loginId":"9f1a2b3c-...."}}

# Complete the 2FA login with the code
curl -s -X POST http://localhost:8080/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"you@example.com","password":"hunter2","code":"123456"}'
# {"code":0,"msg":"SUCCESS","data":{"loginId":"9f1a2b3c-...."}}

# Fetch a playlist once the instance is up
curl -s 'http://localhost:8080/m3u8?adamId=1608815075'
# {"code":0,"msg":"SUCCESS","data":{ ... lite envelope ... }}

# Log the account out
curl -s -X POST http://localhost:8080/logout \
  -H 'Content-Type: application/json' \
  -d '{"username":"you@example.com"}'
# {"code":0,"msg":"SUCCESS","data":{"username":"you@example.com"}}
```

## Data layout

Everything lives under `data/` relative to the working directory:

```
data/
├── wrapper/                                    # wrapper-lite payload (auto-downloaded)
│   ├── wrapper-lite-rootless                   # rootless launcher (login + service entry)
│   ├── wrapper-lite
│   └── rootfs/                                 # chroot userland used by the launcher
│       ├── system/bin/lite                     # the actual lite binary
│       └── data/instances/<instance-id>/       # per-account state (= lite --base-dir)
│           ├── STOREFRONT_ID                   # login artifacts written by lite
│           ├── MUSIC_TOKEN
│           ├── token_cache.json
│           └── 2fa.txt                         # present while a 2FA login is pending
└── instances.json                              # persisted account registry (service mode)
```

- `<instance-id>` is the UUIDv5 of the account username; the same id is the
  manager's `loginId` and the directory name.
- `--base-dir` passed to wrapper-lite is `/data/instances/<id>` (a path inside
  the chroot), which maps to the host path above.
- `instances.json` records `id`/`region`/`port` of logged-in accounts; on
  startup every entry is relaunched in service mode and `/status` is polled
  until its `regions` is non-empty (readiness).

## Deploy (docker compose)

wrapper-lite-rootless creates **user namespaces** and calls **chroot**, which
Docker blocks by default — the compose file runs the container with
`privileged: true`. If you prefer not to use `privileged`, run with a
seccomp/apparmor profile that permits `unshare`/user namespaces instead.

```shell
git clone https://github.com/WorldObservationLog/wrapper-manager
cd wrapper-manager
docker compose up -d
```

- The HTTP API is published on `localhost:8080`.
- A named volume is mounted at `/root/data`, so account state and the
  wrapper-lite payload survive container restarts.
- The compose command passes `--host 0.0.0.0` (the binary default binds to
  `localhost`, which is useless inside a container).

For users in China: uncomment the Go module proxy line in `Dockerfile` and add
`--mirror` to the compose `command` to fetch wrapper-lite through gh-proxy.

## Related projects

- [WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper) — upstream wrapper / wrapper-lite (branch `lite`).
- [WorldObservationLog/AppleMusicDecrypt](https://github.com/WorldObservationLog/AppleMusicDecrypt) — decryptor that consumes this manager's `/m3u8` `/key` `/license` API.
