# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go gRPC server that manages multiple `wrapper` instances (the Apple Music decryption binary from `WorldObservationLog/wrapper`). Each wrapper is a logged-in Apple Music account running in its own process; the manager routes decrypt/m3u8/lyrics/license requests to whichever instance can serve a given track based on its storefront region. Companion client: `WorldObservationLog/AppleMusicDecrypt` (`tools/login.py`).

Runs Linux x86_64 / arm64 only (it shells out to the platform `wrapper` binary and requires `root`).

## Commands

```bash
go build .                          # build (CI uses this)
go run . -host 0.0.0.0 -port 8080   # run (must be root; downloads wrapper + storefront ids on first run)
go run . -prepare                   # only download required files into data/, then exit
docker compose up                   # build + run in container

go mod tidy                         # sync deps
```

No test suite exists (no `*_test.go` files). `go vet ./...` and `gofmt` are the available checks.

Key flags: `-proxy` (HTTP proxy for both wrapper and the manager's outbound Apple API calls), `-mirror` (route GitHub downloads through `gh-proxy.com` for CN users), `-device-info` (passed to wrapper as `-I`), `-debug` (logs wrapper stdout).

### Regenerating protobuf

`proto/manager.proto` is the API contract. The generated `proto/manager.pb.go` and `proto/manager_grpc.pb.go` are committed. After editing the proto, regenerate with `protoc` + `protoc-gen-go` / `protoc-gen-go-grpc`. Note the `go_package` is `wrapper-manager/proto;proto` but the module is `github.com/WorldObservationLog/wrapper-manager` — imports use the full module path.

## Architecture

### Instance lifecycle (`wrapper.go`, `instance.go`)
- An instance ID is a deterministic UUIDv5 of the Apple ID username (fixed namespace `77777777-...`). The same account always maps to the same ID, used both as the on-disk directory name and the in-memory key.
- `WrapperInitial` (new login) and `WrapperStart` (restart a saved account) launch `./wrapper` under a **PTY** (`creack/pty`), each on two randomly-assigned free ports — a decrypt port and an m3u8 port (`port.go`).
- `handleOutput` scans wrapper stdout line-by-line and drives the state machine by string-matching log lines: `"Waiting for input..."` → 2FA prompt, `"listening m3u8 request on"` → ready, `"login failed"` → failure, `"No Active Subscription"` → drop account.
- `wrapperReady` reads the wrapper's `STOREFRONT_ID` file, resolves it to a region code (`parseStorefrontID` against `data/storefront_ids.json`), registers the instance in two places: the global `Instances` slice and the decrypt `WMDispatcher`.
- `wrapperDown`: when a wrapper process exits, it is **auto-restarted** unless `NoRestart` is set (logout, or initial login that never succeeded). State persists to `data/instances.json`.

There are **two parallel registries** of running accounts, kept in sync by `wrapperReady`/`wrapperDown`:
1. `Instances []*WrapperInstance` (`instance.go`) — used by m3u8/lyrics/license/webplayback request paths.
2. `WMDispatcher.Instances []*DecryptInstance` (`decrypt.go`) — used only by the streaming Decrypt path, holding live TCP connections to each wrapper's decrypt port.

When touching instance add/remove, update **both** or routing will desync.

### Request paths (`main.go` implements the gRPC service)
- **Login / Logout / Status** — manage instances. Login is a bidirectional stream; 2FA codes are delivered back to the wrapper by writing `2fa.txt` into the instance dir (`provide2FACode`). Pending login streams are tracked in `LoginConnMap` and resolved by the `handler.go` callbacks.
- **Decrypt** — bidirectional stream. Each sample becomes a `Task` pushed through `WMDispatcher.Submit`. `KEEPALIVE` adam IDs are short-circuited. Region availability is checked before dispatch.
- **M3U8 / Lyrics / License / WebPlayback** — unary. They call `SelectInstance` / `SelectInstanceForLyrics` (`region.go`) to pick an instance whose region can serve the track, then talk to either the wrapper (m3u8 over the instance's TCP port, `m3u8.go`) or Apple's `amp-api`/`play.*` HTTP endpoints directly (`lyrics.go`, `webplay.go`).

### Region selection & decrypt dispatch
- `checkAvailableOnRegion` (`region.go`) hits `amp-api.music.apple.com` to test whether an adam ID exists in a region, memoized in `SongRegionCache` and deduped with `singleflight`. `SelectInstance` prefers songs, falls back to checking music-videos.
- `Dispatcher.selectInstance` (`decrypt.go`) picks a decrypt instance by, in order: one already keyed to this adam ID, then any idle+available, then a random available one. Decryption is **stateful** — `DecryptInstance.switchContext` sends the adam ID + key over the raw TCP conn before sending samples, and `connMu` serializes access per instance. Any I/O error marks the instance `Unavailable` (closes conn + kills the wrapper, which then triggers auto-restart via `wrapperDown`).

### Tokens (`token.go`)
- `GetToken` scrapes a bearer JWT from `music.apple.com`'s index JS, cached 24h in an expirable LRU.
- `GetMusicToken` reads the per-instance `MUSIC_TOKEN` file written by the wrapper.

### State on disk (all under `data/`, gitignored)
- `data/wrapper/wrapper` — the wrapper binary; `data/wrapper/rootfs/data/instances/<id>/` — per-account state (`STOREFRONT_ID`, `MUSIC_TOKEN`, `2fa.txt`).
- `data/instances.json` — persisted accounts to restart on boot.
- `data/storefront_ids.json` — storefront → region-code mapping (downloaded from a gist).

## Conventions
- All gRPC replies use a `ReplyHeader{code, msg}`: `0` = SUCCESS, `-1` = error (message in `msg`), `2` = 2FA required. Errors are returned in-band on the reply, not as gRPC status errors.
- Wrapper protocol over TCP is hand-rolled: a 1-byte length prefix followed by the string for adam ID / key (decrypt), or length-prefixed sample with a little-endian uint32 length (sample payloads). Don't change framing without matching the wrapper side.
- The codebase `panic`s liberally on filesystem/setup errors during startup; request handlers return errors instead.
