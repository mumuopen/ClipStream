# ClipStream ⚡

> **Live auto-record · Danmaku-aware smart clipping · Translate & dub · Multi-platform auto-publish**
> **直播自动录制 · 弹幕驱动智能切片 · 翻译配音 · 多平台自动发布**

A Go reimagination of [ChopperBot](https://github.com/Geniusay/ChopperBot) — one pipeline from live stream to published video. 一条从直播流到已发布视频的完整流水线。

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue)](LICENSE)
[![PRs](https://img.shields.io/badge/PRs-welcome-brightgreen)](https://github.com/mumuopen/ClipStream/issues)
[![ChopperBot](https://img.shields.io/badge/inspired%20by-ChopperBot-ff69b4)](https://github.com/Geniusay/ChopperBot)

---

## ✨ 特性亮点 / Highlights

| | 能力 / Capability | 说明 / Notes |
|---|---|---|
| 🎥 | **直播链 Live chain** | B站 + 虎牙开播检测、分段录制、断流续接、下播自动切片 / Bilibili & Huya detection, segment recording, reconnect, auto-clip |
| 💬 | **弹幕驱动 Danmaku** | WebSocket 采集 + 5s 密度桶 + 实时曲线 / WS collection → density buckets → live curve |
| 🎯 | **双信号高光 Dual-signal** | ASR 转写 × 弹幕热点 → LLM 选段；无 LLM 弹幕热点直切 / transcript × barrage peaks |
| 📖 | **字幕 Subtitles** | LLM 翻译 + 双语/中文烧录 / LLM translate + burn-in |
| 🎙️ | **配音 Dub** | edge-tts 解说合成、变速对齐 / narration synthesis & alignment |
| 📦 | **发布 Publish** | B站 biliup 投稿、自动审核分级闸门（可关）、内容池错峰发布、PushMe 通知 / auto-review gates, off-peak publish |
| 🌐 | **界面 Console** | 中英双语、记忆选择 / bilingual UI, remembered choice |

> 更多平台（抖音 / 小红书 / 快手）已规划，按 Issue 排期 / More platforms scheduled via Issues.

---

## 🖼️ 界面预览 / Screenshots

| 总览 Dashboard | 直播关注 Live Follow | 弹幕曲线 Barrage Curve |
|:---:|:---:|:---:|
| ![board](docs/screenshots/shot-board.png) | ![live](docs/screenshots/shot-livefollow.png) | ![barrage](docs/screenshots/shot-barrage.png) |

| 审核台 Review | 标签矩阵 Label Matrix | English UI 🇬🇧 |
|:---:|:---:|:---:|
| ![review](docs/screenshots/shot-review.png) | ![labels](docs/screenshots/shot-labels.png) | ![board-en](docs/screenshots/shot-board-en.png) |

---

## ⚡ 快速开始 / Quick Start

```bash
# 依赖 Dependencies：Go 1.21+；可选引擎（缺失自动降级）
#   yt-dlp / ffmpeg / ffprobe / whisper.cpp / biliup / LLM(OpenAI 兼容, 如 DeepSeek)

# 构建 Build
go build -o bin/clipstream.exe ./cmd/clipstream
go build -o bin/clipstream-daemon.exe ./cmd/clipstream-daemon

# 初始化 Init
cp config.example.yaml config.yaml      # 填引擎路径与 LLM key / fill engines & LLM key
./bin/clipstream init

# 控制台 Console
./bin/clipstream serve                  # → http://127.0.0.1:8899

# 直播守护 Live daemon（独立进程 separate process）
./bin/clipstream-daemon

# CLI 任务 Run a job
./bin/clipstream run -i "https://www.bilibili.com/video/BVxxxx"
```

---

## 🏗️ 架构 / Architecture

![architecture](docs/architecture.png)

- **纯 Go，单二进制**，SQLite 无 cgo（modernc.org/sqlite）
- **进程分离**：录制守护与 Web/管线各自独立进程，规避 Windows 父进程拉起子进程的 FLV 流式 IO 坑
- **引擎全部外挂、优雅降级**：缺 whisper → 跳过转写；缺 LLM → 等分/弹幕热点切片
- **无人值守闭环**：自动审核分级闸门 → 内容池 → 错峰调度（平台 × 标签账号路由）→ 发布 → PushMe 通知 + 每日战报
- 弹幕：B站 WebSocket（wbi + uid=0）、虎牙 va 节点订阅，均 5s 桶落库

> 图源文件 `docs/architecture.drawio`，可在 [app.diagrams.net](https://app.diagrams.net) 打开编辑。

---

## 📂 目录 / Layout

```
├── cmd/clipstream           主程序 main (serve/run/init/login/status)
├── cmd/clipstream-daemon   直播录制守护 live-recording daemon
├── internal/config         配置 config
├── internal/core           任务模型与事件总线 job model & event bus
├── internal/engine         引擎封装 engine adapters
├── internal/live           直播链：B站/虎牙 live chain
├── internal/pipeline       五类管线 pipelines
├── internal/store          SQLite 存储 storage
├── internal/web            控制台 console (embedded)
└── configs/platforms       平台配置 platform configs
```

---

## 📖 文档 / Docs

- [English README](README.en.md) · [CHANGELOG](CHANGELOG.md) · [配置示例 / config example](config.example.yaml)

## 💬 反馈 / Feedback

- Bug / 功能需求：**Issue** 即可（平台接入、新引擎、发布渠道…）· PRs welcome

## 🙏 致谢 / Credits

本项目为 [Geniusay/ChopperBot](https://github.com/Geniusay/ChopperBot)（Apache-2.0）**模块思想启发的 Go 独立重写** —— 特此致敬原作者；原版源码仅供研读，未随本仓库分发。依赖引擎/库各遵守其许可证。

> **"Stream in, videos out."**