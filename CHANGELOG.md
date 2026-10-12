# Changelog

ClipStream 遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与语义化版本（SemVer）。本文件使用中文记录。

## [0.3.0] - 2026-10-12

### 开源发布

- 开源收口：Apache-2.0 License、中英 README、配置示例、仓库发布
- 品牌统一为 **ClipStream**（原内部代号 GuanClip）

### 新增

- **M4.2 弹幕链**：B站 + 虎牙双平台弹幕 WebSocket 采集，5s 密度桶落库
- **M4.2 双信号高光**：ASR 转写 + 弹幕热点融合选段（LLM 优先采纳互动高峰区间；无 LLM 时弹幕热点直接切片）
- **M4.1 多平台直播链**：B站 + 虎牙开播检测 / 自动录制 / 断流续写 / 下播自动切片
- **M5 矩阵骨架**：多账号管理（B站）、发布调度占位、路由规则说明
- 控制台：直播关注、弹幕曲线、标签矩阵页
- 字幕（LLM 翻译 + 烧录）/ 配音（edge-tts 解说）管线

### 修复

- 直播录制守护进程独立运行（绕开 Windows 父进程拉起子进程的 FLV 流式 IO 问题）
- 控制台列表 API 空库时返回 `null` 导致前端崩溃 → 空数组兜底
- 控制台 HTML 增加 `Cache-Control: no-store`（避免浏览器缓存旧版误导调试）

### 变更

- 账号仅支持 B站（biliup）；抖音/小红书/快手经 Issue 排期

## [0.2.0] - 2026-10-10

### 新增

- M2 发布链路：B站 biliup 投稿、抖音 rod 浏览器发布（选择器配置化）、dry-run 演练
- M3 字幕与配音：DeepSeek 翻译 + SRT + 烧字幕；edge-tts 解说配音
- 审核台：切片人工过审后才可发布
- pushme 手机通知（审核/发布/失败）

## [0.1.0] - 2026-10-08

### 新增

- M0/M1 骨架：任务队列（SQLite）、CLI（serve/run/init/status）
- 五类管线：clips（下载→转写→高光→切片→审核）、发布、字幕、配音、直播
- 引擎外挂与优雅降级：yt-dlp / ffmpeg / whisper.cpp / LLM（DeepSeek）
- 控制台第一版（总览/任务中心/审核台/设置）

---

[Keep a Changelog]: https://keepachangelog.com/zh-CN/1.1.0/
[SemVer]: https://semver.org/lang/zh-CN/