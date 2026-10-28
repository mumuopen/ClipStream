# HANDOFF — ClipStream 交接文档

> 新 agent 从这里开始。本文档记录"当前活状态"与接手路径；设计演进史读 CHANGELOG（倒序），运维全貌读 `temp_server\部署说明.txt`。
> 最后更新：2026-10-28（v0.6.5）

## 项目一句话

ClipStream：Go 编写的直播切片自动发布平台（直播监控→分段录制→ASR+LLM 切片→审核→发布泵匀速发 B站/抖音）。
仓库 `E:\dev\ChopperBot`（GitHub mumuopen/ClipStream）；生产服务器 `D:\clipstream`（Windows）；本地部署包 `temp_server\`（gitignore 内，含真实 key 勿外传）。

## 接手路径（按顺序）

1. **CHANGELOG.md 倒序读最近三版**（0.6.5/0.6.4/0.6.3）——每段都带事故背景，是当前架构"为什么长这样"的最快入口
2. **代码主链**（注释密度极高，设计决策与事故教训全部写在注释里，改代码前必读）：
   - `cmd/clipstream/main.go` — 发布泵 `pumpTick`、四闸 `publishThrottled`、调度 `scheduleTicker`
   - `internal/store/store.go` — 全部表结构与迁移（幂等 ALTER）、清理三闸、风控冷却
   - `internal/pipeline/stages_publish.go` — 发布失败善后（finalize 必然执行：停号/熔断/回池）
   - `internal/web/server.go` + `internal/web/console.html` — 全部 API 与单文件前端
3. `temp_server\部署说明.txt` — 服务器运维全貌（节奏参数、风控熔断、清理、背压、磁盘治理）

## 当前活状态（2026-10-28）

- 版本 **0.6.5**，Release v0.6.5 = Latest；`temp_server\bin\` 双 exe 已同步
- **服务器已停 serve**（风控冷却中）：B站账号 10-24 触发 137022"投稿过于频繁"（详见 CHANGELOG 0.6.1 背景）
- **服务器待办**（用户自己执行，agent 不碰服务器）：
  1. 换血 `temp_server\bin\` 双 exe（0.6.5）；config.yaml **按需**覆盖（已调过的发布节奏不要整包覆盖）
  2. 看板 → 发布中心 → 一键清理 → 选【确定】连待发池清空（老 pending 占着背压水位）
  3. 风控冷却过了再开：`active_hours` 填保守窗口、`min_interval_min: 30-60`、`daily_max: 9-20`
  4. 观察熔断链路首次真实触发（通知+账号卡徽标）
- **0.6.3 关键变更**（换血后行为变化）：手动发布现在也受风控冷却闸（冷却期点发布会被 429 拒）；
  删一条已成功的发布任务**不再**把切片退回待发池；崩溃重启的发布任务一律留 dispatched 等人工核对；
  背压期下播的分段会登记并在池子消化后自动补送
- 历史事故：0.6.1 一键清理误删审核台待审字幕素材（0.6.2 修）；0.6.3 成品退役死结导致 `data/jobs` 只进不出（已修）

## 核心机制地图

- **发布模型**：无定时计划，池子 + 发布泵（30s tick，窗口内每 tick 至多派 1 条）+ `active_hours` 窗口
- **四闸**（`publishThrottled`，全在手动+泵路径上）：⓪风控冷却（限流触发即账号级冷却，指数退避 24→48→96h 封顶 7 天）→ ①间隔（10min ±5% 抖动）→ ②每日上限 → ③同时在途
- **风控熔断**（0.6.1）：`publish.IsRateLimited` 识别限流文案 → 账号 `risk_paused_until` 落库 → 泵/手动全拦 → 到点自动恢复；账号卡可手动恢复（计数保留）。**限流失败不计条目 3 次退池**（是账号问题不是内容问题）
- **失败善后**（finalize）：登录失效→停号 login_expired；限流→熔断；其他失败→回池，连续 3 次置 failed 终态退池（红色"已退池"等人工）
- **背压**：池 pending ≥ `pool_high`(20) 暂停录制与送切，降回 `pool_low`(10) 恢复；滞回双水位。⚠️ 与风控冷却叠加会死锁（池子发不出去→水位不退→录制永停），一键清理"清空待发池"是解法
- **一键清理**（发布中心按钮）：预览→确认→清已完结池条目+终态任务（记录+产物目录），可选清空待发池；三道保护：池 pending 引用、非终态任务引用（字幕/配音成品与在途发布引用源目录）、目录必须在 `<DataDir>/jobs` 下
- **账号路由**：直播绑定优先（watches.bind_account）→ 标签回退；扫码登录 cookie 落 `data\accounts\`
- **磁盘治理**（daemon）：超期会话→已发布成品退役（3 天）→水位 10%→占用上限 50%，正在录制绝不碰

## 开发流程纪律（每轮改动照此走）

1. 改代码 → `go build ./...` → `go test ./...` 全绿
2. **拟真验证**：独立目录 `.temp\sim-*/`（写最小 config、造数据、起 serve）——**绝不动 `temp_server\data\`**（可能存有用户拖回的服务器真实数据）
3. 构建：`go build -trimpath -ldflags "-s -w" -o temp_server\bin\<双 exe> ./cmd/<对应包>`
4. 提交：详细中文提交信息（**引号必须全角**——半角双引号会截断 git -m 参数报 pathspec 错）
5. push：代理 7897 断时用 `git -c http.proxy= -c https.proxy= push`
6. Release：gh token 从 remote URL 正则 `://[^:]+:([^@]+)@` 提取且**同进程**设置 GH_TOKEN；附件一律 ASCII 名（中文名 404）；大 exe 分开上传给足超时

## 坑清单（前人踩过，别再踩）

- **store 是单连接池**（`SetMaxOpenConns(1)`）：rows 迭代中嵌套查询 = 永久死锁——嵌套需求改成两条 SQL 或 NOT EXISTS
- **任务日志在 sqlite** `jobs.log` 字段（每次 AppendLog 截半防膨胀），不在任何文件；job 目录只有素材（publish 任务的目录是空的，属正常）
- **外来 sqlite 先隔离 wal/shm 再打开**（拟真残留 wal 会重放污染拖回的服务器库——3MB 真数据被覆盖成 564KB 的事故）；查询一律 `mode=ro`
- Playwright 连的是用户共享浏览器实例：UI 实点验证可能被用户操作打断（弹窗丢失），关键验证用 API 直调兜底
- PowerShell 5.1：无 `&&`；中文 body 必须 UTF8 字节流（`[Text.Encoding]::UTF8.GetBytes`）；python 单行脚本引号嵌套易翻车，一律写脚本文件
- 后台进程管理走 `.temp/background-registry-*.tsv` 注册表流程（start→inspect 核对 purpose→cleanup），绝不裸 Start-Process
- **拟真一律先 Chdir**：验证程序用相对 `./data` 而 `go run` 的工作目录是仓库根——不切目录就会往开发库 `data/clipstream.db` 写脏数据（2026-10-28 踩过，已清理）

## 敏感事项

- `temp_server\config.yaml` 含真实 LLM/PushMe key——勿外传、勿提交（temp_server 在 gitignore）
- 控制台只听 127.0.0.1（无鉴权），公网暴露风险自担
- B 站账号有封禁史（9 连发被封过一次）：发布节奏任何调快都要过用户拍板，10min×日不限的档位已实测触发平台限流

## 历史

详细事故复盘（风控 137022、清理误删、finalize 断链等）都在 CHANGELOG 各版"背景"段；更细的过程记录在 TeleAgent 每日日志（`daily-log/2026-10-24.md`、`2026-10-25.md`）。
