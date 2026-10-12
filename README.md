# GuanClip — ChopperBot 的 Go 重写融合平台

> 直播录制 · 智能切片 · 搬运下载 · 翻译配音 · 多平台自动发布，一体化。

**致敬与致谢**：本项目是 [Geniuszy/ChopperBot](https://github.com/Geniuszy/ChopperBot)（Apache-2.0，Java 直播切片机器人）的 **Go 重写与融合版**。模块地图（Board / Task Center / Live Follow / Barrage Curve / Label Manage 等）、双信号高光思路、多平台发布思想均借鉴自原版，特此致谢。原版仓库位于 `reference/ChopperBot`（仅供研读，未随本仓库分发）。

---

## 它是什么

一条**直播 → 切片 → 发布**的自动化流水线：

```
开播检测 → 自动录制(分段) → 弹幕采集(密度桶) → 高光(ASR 转写 + 弹幕热点双信号)
         → 切片 → 人工审核 → B站投稿(biliup) → 推送通知(pushme)
```

以及独立的**搬运链**：任意视频 URL（YouTube / B站 / 抖音…）→ 下载 → 转写 → 高光 → 切片 → 字幕/配音 → 审核 → 发布。

## 一、当前支持

| 能力 | 状态 |
|---|---|
| 视频下载 / 转写 / 高光 / 切片 / 审核 | ✅ |
| 直播录制（**B站 + 虎牙**：开播检测、分段、断流续写） | ✅ |
| 弹幕密度采集（**B站 + 虎牙**，5s 桶实时曲线） | ✅ |
| 双信号高光融合（ASR 转写 + 弹幕热点 → LLM 选段） | ✅ |
| 字幕（DeepSeek 翻译 + 烧录） / 配音（edge-tts 解说） | ✅ |
| 发布（**B站**：biliup 投稿；审核台 dry-run 演练） | ✅ |
| 矩阵账号 / 发布调度 | 骨架（账号仅 **B站**） |

其他平台（抖音/小红书/快手）发布、账号登录等**尚未接入**，按用户需求通过 Issue 排期。

## 二、快速开始

```bash
# 1. 环境：Go 1.22+；可选引擎（缺省自动降级）：
#    yt-dlp / ffmpeg / ffprobe / whisper.cpp(ASR) / biliup(B站发布) / LLM(DeepSeek 等 OpenAI 兼容)

# 2. 初始化
go build -o bin/guanclip.exe ./cmd/guanclip
go build -o bin/guanclip-daemon.exe ./cmd/guanclip-daemon
cp config.example.yaml config.yaml    # 按需填写引擎路径与 LLM key
./bin/guanclip init

# 3. 控制台
./bin/guanclip serve                  # http://127.0.0.1:8899

# 4. 直播录制守护进程（独立进程，与 serve 分离）
./bin/guanclip-daemon

# 5. 命令行跑一个任务（视频 URL 或本地文件）
./bin/guanclip run -i "https://www.bilibili.com/video/BVxxxx"
```

控制台包含：总览 / 任务中心 / 审核台 / 直播关注 / 弹幕曲线 / 标签矩阵 / 设置。

## 三、使用要点

- **发布需人工审核**：切片产出进入「审核台」，发布前必须过审（默认 dry-run 演练模式，确认后真发）。
- **B站登录**：`./bin/guanclip login --platform bilibili`（biliup 弹窗扫码）；登录态存于本地 `data/accounts/`（不入库、不上传）。
- **直播录制**：控制台「直播关注」添加 B站/虎牙直播间 → daemon 自动检测开播/录制/下播切片。
- **LLM 可缺省**：未配置 api_key 时高光自动回退为等分切片或弹幕热点切片，全链可跑。
- **配置**：`config.yaml`（已在 .gitignore，含密钥不入库）。

## 四、技术要点

- 纯 Go，单二进制；SQLite（modernc.org/sqlite，无 cgo）。
- 引擎全部外挂：yt-dlp / ffmpeg / whisper.cpp / biliup / LLM，均支持优雅降级。
- 直播录制守护进程（`guanclip-daemon`）与 Web 控制台（`serve`）**进程分离**：绕开 Windows 下父进程拉起子进程的 FLV 流式 IO 问题（详见代码注释）。
- 弹幕：B站走 WebSocket（wbi 签名 + uid=0 认证），虎牙走 va 边缘节点订阅协议（baseinfo + live/chat 频道），均按 5s 密度桶落库。
- 双信号高光：ASR 转写（LLM 语义）+ 弹幕密度（观众互动峰值）融合选段，无 LLM 时弹幕热点直接切片。

## 五、目录结构

```
cmd/guanclip          主程序（serve/run/init/login/status）
cmd/guanclip-daemon   直播录制守护进程
internal/config       配置（config.example.yaml）
internal/core         任务模型与事件总线
internal/engine       yt-dlp / ffmpeg / whisper / LLM / biliup 封装
internal/live         直播链：B站/虎牙 API、录制、弹幕采集（B站 WS + 虎牙 TACG/va）
internal/pipeline     五类管线（clips/publish/subtitle/dub/live）
internal/store        SQLite（jobs/watches/barrage/accounts/schedules）
internal/web          控制台（单页 embed）
configs/platforms     发布平台选择器配置
```

## 六、License

[Apache-2.0](LICENSE)。

- 本项目为 **ChopperBot 的独立重写**：代码为全新 Go 实现，模块设计/思路受 [Geniuszy/ChopperBot](https://github.com/Geniuszy/ChopperBot)（Apache-2.0）启发，特此致谢。
- 依赖的引擎与库遵守各自许可证（biliup / whisper.cpp / yt-dlp / rod 等）。

## 七、反馈

- Bug / 功能需求：欢迎在 GitHub **Issue** 留言（新平台接入、新引擎、新发布渠道等）。
- 本项目不定期迭代，不承诺具体交付排期。