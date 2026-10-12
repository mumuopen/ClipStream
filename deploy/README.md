# 目标机部署指南（Windows 挂机）

## 一、制作部署包（在开发机执行）

```powershell
# 在仓库根目录：
.\deploy\make-package.ps1
# 产物：.temp\clipstream-deploy.zip（源码 + 脚本，不含数据/密钥）
```

## 二、目标机准备

1. 安装 **Go 1.22+**（https://go.dev/dl/，或用 winget：`winget install GoLang.Go`）
2. 安装可选引擎（缺哪个就降级哪条链）：
   - `yt-dlp`、`ffmpeg`、`ffprobe`（放进 PATH 或填 config 绝对路径）
   - `biliup.exe` → `<部署目录>\engines\biliup.exe`（B站发布必需，github.com/biliup/biliup releases）
   - whisper.cpp（离线转写，可选）
3. 解压 `clipstream-deploy.zip` 到任意目录，如 `C:\clipstream`

## 三、配置

```powershell
cd C:\clipstream
copy config.example.yaml config.yaml
notepad config.yaml
```

必填：
- `llm.api_key`（DeepSeek 等 OpenAI 兼容；不填则高光降级为等分/弹幕切片）
- `notify.push_key`（手机 PushMe，开机验证用）
- `engines.biliup`（绝对路径）

## 四、构建 + 首次登录（B站）

```powershell
# 构建
go build -o bin\clipstream.exe .\cmd\clipstream
go build -o bin\clipstream-daemon.exe .\cmd\clipstream-daemon

# B站账号扫码登录（一次即可，cookie 存 data\accounts\）
.\bin\clipstream.exe login --platform bilibili

# 引擎体检
.\bin\clipstream.exe init
```

## 五、挂机启动

**手动启动（先试用）**：双击 `deploy\start-clipstream.cmd`（幂等，重复点不重复启）

**开机自启**：
1. `Win+R` → `shell:startup` 回车，打开启动文件夹
2. 右键 `deploy\start-clipstream.cmd` → 发送到桌面快捷方式
3. 把快捷方式剪切进启动文件夹
4. 重启验证：开机后任务管理器出现 `clipstream.exe` + `clipstream-daemon.exe`，手机收到 pushme 通知

（若目标机可以用管理员 PowerShell，也可以用任务计划程序注册，开机不登录即跑：
`Register-ScheduledTask` 指向该 cmd，触发器 AtStartup；本仓库不提供提权脚本）

## 六、验证清单

| 项 | 方法 |
|---|---|
| serve | 浏览器开 `http://127.0.0.1:8899`（或局域网 IP:8899，注意防火墙放行） |
| daemon | 控制台「直播关注」添加直播间，看状态变「录制中」 |
| pushme | 手机收到「开播提醒」即通知链路通 |
| 磁盘清理 | daemon.log 出现 `[cleanup]` 行；`config.retention` 控制保留天数 |
| 错峰发布 | 审核台「入池待发」→ 标签矩阵建调度 → 到点自动 biliup 投稿 → pushme 派发通知 |

## 七、日常运维

- 日志：`data\serve.log`、`data\daemon.log`（追加式，定期清）
- 数据：全部在 `data\`（SQLite + recordings + 任务产物）；备份/迁移拷贝整个目录即可
- 升级：开发机出新包 → 停进程（任务管理器结束两个 exe）→ 覆盖 `bin\` 与源码 → 重跑构建 → 启动