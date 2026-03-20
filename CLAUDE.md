# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# offtube

A single-binary Go app that acts as an offline YouTube client. It starts an HTTP server with an API to submit downloads and serves the downloaded content.

## Build & Run

```bash
make build          # build for current platform
make build-arm64    # cross-compile for RPi4 (linux/arm64)
make docker         # build Docker image
make run            # go run .
make test           # go test ./...
go test ./... -run TestName   # run a single test
```

**Dependencies:** `yt-dlp` and `ffmpeg` must be installed and on `$PATH` for the app to work. Tests do not require them.

## Architecture

Single Go binary using only the standard library. HTML templates and static assets (video.js) are embedded via `//go:embed`.

**Data layout** — each downloaded item lives under `data/<youtube_id>/`:
```
data/<youtube_id>/
    metadata.txt    ← key=value fields; description newlines escaped as \n
    log.txt         ← appended yt-dlp output (each run gets a timestamped separator)
    video.mp4       (video downloads)
    video.jpg       (thumbnail)
    audio.mp3       (audio-only downloads)
    audio.jpg       (thumbnail)
```

Legacy `metadata.json` files are auto-migrated to `metadata.txt` on startup.

**App struct** (`main.go`) holds:
- `items map[string]*Metadata` — in-memory index, populated from disk on startup, protected by `sync.RWMutex`
- `jobQueue chan *Metadata` — buffered channel feeding `maxExecutors` worker goroutines

## Download queue

- `maxExecutors` (constant) — max concurrent yt-dlp processes
- `jobQueueSize` (constant) — capacity of the job channel
- On submit: metadata is saved to disk with `status=queued`, then sent to `jobQueue`
- Workers (`worker()` goroutines) pull from the channel and call `download()`
- If all workers are busy the channel blocks the submitter until a slot is free (up to `jobQueueSize` capacity before the HTTP handler itself blocks)

## Download statuses

| Status | Meaning |
|--------|---------|
| `queued` | waiting for a free executor slot |
| `running` | yt-dlp process is active |
| `done` | finished successfully |
| `error` | yt-dlp exited non-zero |
| `interrupted` | app restarted while job was queued or running |

On startup, any item with status `queued` or `running` is marked `interrupted`. There is **no automatic retry**; the user must click Retry on the media details page (shown for `error` and `interrupted` states).

## Key constants (top of `main.go`)

| Constant | Default | Purpose |
|----------|---------|---------|
| `maxExecutors` | 1 | max concurrent yt-dlp processes |
| `jobQueueSize` | 100 | download channel buffer capacity |
| `ytdlpAutoUpdate` | false | enable periodic `yt-dlp -U` |
| `ytdlpUpdateInterval` | 24h | how often `yt-dlp -U` runs |
| `pageSize` | 20 | items per page on the list view |
| `cleanupEnabled` | false | enable periodic removal of disliked media |
| `cleanupInterval` | 24h | how often cleanup runs |

## API endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/submit` | `{"url":"…","type":"video"\|"audio"}` → enqueue download |
| `POST` | `/api/retry/{id}` | re-enqueue a failed/interrupted download |
| `POST` | `/api/like/{id}` | `{"value": 1\|-1\|0}` → update like state |
| `GET`  | `/api/status/{id}` | returns `{"status":"…","log":"…"}` |
| `POST` | `/api/webhook/miniflux/video` | Miniflux webhook → enqueue as video |
| `POST` | `/api/webhook/miniflux/audio` | Miniflux webhook → enqueue as audio |

**Miniflux webhook** handles `new_entries` (array) and `save_entry` (single) event types. Duplicates are logged and skipped; response is always 200.

**Duplicate detection:** `submitURL()` runs `yt-dlp --dump-json --no-download` first to get the YouTube ID, then checks if that ID already exists in `items`.

## Frontend

- **List page** — vertical item list with thumbnail, title, type/status badges, like/dislike labels; client-side search + type filter (All/Video/Audio chips); client-side pagination (`PAGE_SIZE` injected from `pageSize` constant); live badge updates via `/api/status` polling for queued/running items every 4s
- **Media page** — 3 tabs: Player (video.js for video, `<audio>` for audio), Metadata, Log; transport controls (−5s / play-pause / +5s); playback position persisted in cookie (`pos_<id>`); Retry button for `error`/`interrupted`; log tab auto-polls while in progress
- **video.js** — vendored at `static/video-js/` (embedded in binary)

## Docker

Multi-stage build: `golang:1.22-alpine` compiles the binary; `alpine:3.19` final image adds `ffmpeg`, `yt-dlp`, and `deno` (all from Alpine community repo).

```bash
docker run -p 8080:8080 -v $(pwd)/data:/data offtube
docker build --platform linux/arm64 -t offtube:arm64 .   # for RPi4
```
