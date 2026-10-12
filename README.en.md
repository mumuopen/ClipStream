# ClipStream

> One pipeline for live recording · smart clipping · download & port · translate & dub · multi-platform auto-publishing.

**Credits**: This project is a **Go rewrite & fusion** of [Geniuszy/ChopperBot](https://github.com/Geniuszy/ChopperBot) (Apache-2.0, a Java live-clip robot). Module map (Board / Task Center / Live Follow / Barrage Curve / Label Manage ...), dual-signal highlight ideas, and multi-platform publishing concepts are inspired by the original. The original source is for study only and is **not** distributed with this repo.

## Screenshots

| Dashboard | Live Follow (auto record) | Barrage Curve |
|---|---|---|
| ![board](docs/screenshots/shot-board.png) | ![live](docs/screenshots/shot-livefollow.png) | ![barrage](docs/screenshots/shot-barrage.png) |

| Review (human gate before publish) | Label Matrix (accounts & scheduling) |
|---|---|
| ![review](docs/screenshots/shot-review.png) | ![labels](docs/screenshots/shot-labels.png) |

## What it does

A **live → clip → publish** automation pipeline:

```
live-detection → auto-record(segments) → danmaku-density buckets → highlight(ASR + barrage hotspots)
              → cut → human review → publish to Bilibili (biliup) → push notification
```

Plus a **port/download chain**: any video URL (YouTube / Bilibili / Douyin ...) → download → transcript → highlight → clip → subtitle/dub → review → publish.

## Current support

| Capability | Status |
|---|---|
| Download / Transcribe / Highlight / Clip / Review | ✅ |
| Live recording (**Bilibili + Huya**: detection, segments, reconnect) | ✅ |
| Danmaku density (Bilibili + Huya, 5s buckets, live curve) | ✅ |
| Dual-signal highlight (ASR + barrage hotspots → LLM selection) | ✅ |
| Subtitles (LLM translate + burn-in) / dub (edge-tts narration) | ✅ |
| Publish (**Bilibili**: biliup upload; review-gate dry-run) | ✅ |
| Matrix accounts / publish scheduling | skeleton (Bilibili only) |

Other platforms (Douyin / Xiaohongshu / Kuaishou) are **not yet wired**; they will be scheduled via Issues.

## Quick start

```bash
# 1. Go 1.22+; optional engines (auto-degrade when missing):
#    yt-dlp / ffmpeg / ffprobe / whisper.cpp(ASR) / biliup(Bilibili publish) / LLM(DeepSeek etc., OpenAI-compatible)

# 2. Build
go build -o bin/clipstream.exe ./cmd/clipstream
go build -o bin/clipstream-daemon.exe ./cmd/clipstream-daemon

# 3. Init
cp config.example.yaml config.yaml    # fill engine paths & LLM key as needed
./bin/clipstream init

# 4. Console
./bin/clipstream serve                # http://127.0.0.1:8899

# 5. Live-record daemon (separate process from serve)
./bin/clipstream-daemon

# 6. Run one task from CLI (URL or local file)
./bin/clipstream run -i "https://www.youtube.com/watch?v=..."
```

Console pages: Dashboard / Tasks / Review / Live Follow / Barrage Curve / Label Matrix / Settings.

## Notes

- **Publishing requires human review**: clips go to the Review page first; real publish only after approval (dry-run by default).
- **Bilibili login**: `./bin/clipstream login --platform bilibili` (biliup QR in a terminal window). Credentials stay under `data/accounts/` (never committed).
- **Live recording**: add a room on the Live Follow page → daemon auto-detects/records/auto-clips when the stream ends.
- **LLM optional**: without an `api_key`, highlight falls back to equal-chunk or barrage-hotspot slicing; the whole pipeline still runs.
- **Config**: `config.yaml` is gitignored (secrets never committed).

## Tech notes

- Pure Go, single binary; SQLite via `modernc.org/sqlite` (no cgo).
- All engines are external: yt-dlp / ffmpeg / whisper.cpp / biliup / LLM, each with graceful degradation.
- The recording daemon runs as a **separate process** from the Web console, avoiding a Windows pitfall where child processes spawned from a heavy parent cannot do FLV streaming I/O.
- Danmaku: Bilibili via WebSocket (wbi signature + uid=0 auth); Huya via va-edge-node subscription protocol (baseinfo + live/chat channels); both stored as 5s density buckets.
- Dual-signal highlight: ASR transcript (LLM semantics) + danmaku density (real-time audience peaks); without an LLM, barrage hotspots become clips directly.

## Layout

```
├── cmd/clipstream          main (serve/run/init/login/status)
├── cmd/clipstream-daemon   live recording daemon
├── internal/config         config (config.example.yaml)
├── internal/core           job model & event bus
├── internal/engine         yt-dlp / ffmpeg / whisper / LLM / biliup adapters
├── internal/live           live chain: Bilibili/Huya API, recording, danmaku
├── internal/pipeline       pipelines (clips/publish/subtitle/dub/live)
├── internal/store          SQLite (jobs/watches/barrage/accounts/schedules)
├── internal/web            console (single-page embed)
└── configs/platforms       platform adapter selectors
```

## License

[Apache-2.0](LICENSE).

This project is an **independent rewrite** of ChopperBot: the code is brand-new Go, inspired by [Geniuszy/ChopperBot](https://github.com/Geniuszy/ChopperBot) (Apache-2.0) — thanks to the original author. Third-party engines/libs keep their own licenses (biliup / whisper.cpp / yt-dlp / rod ...).

## Feedback

- Bugs / feature requests: welcome via GitHub **Issues** (new platform adapters, new engines, new publish channels...).
- Maintained on a best-effort basis; no delivery schedule is guaranteed.