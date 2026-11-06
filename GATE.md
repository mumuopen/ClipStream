# 发布前体检门 · 第 1 期（止血版）改动说明

> 版本 0.7.0。任务书：只做**检测与拦截 + 决策日志**；不抹水印、不做发布后数据回标（第 2 期）。

## 挂载点

- **挂载点 A（主）**：`internal/pipeline/stages.go stageCut` 每片导出循环内 → 调 `internal/gate.Check` 四关。enforce 拦截的切片不入 results（不进审核台/池，文件保留供审计）；crop 处置换用裁切后的素材。
- **挂载点 B（场级基线）**：`internal/live/daemon.go` 分段落地回调 → 异步 `styleCheckAsync`（不阻塞拉流）。首段建基线，后续每段比对（600s 分段 ≈ 每 5-10 分钟一次），超阈值标漂移+推送。
- **人工确认（关卡 4）= 现有审核台**：入池/手动发布回填 `approved`，"不采用"删任务回填 `rejected`（只覆盖仍 pending 的记录）——写回决策日志同一条。

## 开关用法

`config.yaml` 新增 `publish_gate` 段，交付默认 **`enabled: true` + `mode: dry-run`**（只记日志不拦截）；确认阈值调好后改 `mode: enforce` 真拦截。`enabled: false` 时流水线行为与现状完全一致。房间级水印/广告区域按 `rooms.<room_id>` 手工配坐标（模板图从无污染帧截取）。风格漂移人工确认后：`POST /api/gate/rebaseline {"room_id":"..."}` 重建基线清标记。

## 决策日志（评测集地基）

每条候选切片一条 JSONL：`data/gate/decisions.jsonl`。字段：clip_id、live_session_id、时间区间、本场基线版本、四关详情（采样帧数/命中列表：时间点·区域·类型·置信度/漂移标记/分数）、人工处置（action/at/operator）、最终处置、配置版本、耗时。人工回填写回同一条（按 clip_id 定位重写该行）。查询：`GET /api/gate/decisions?clip_id=...`。

## 已知限制与误报情况

1. **模板必须含水印块+少量周边背景**：纯色模板（如完整色块贴片本身）方差为 0，NCC 视为无信息量跳过（回放验收实测踩过）。
2. **NCC 对自相似背景敏感**：周期性纹理/纯色平涂背景可能误报——区域配置收窄到水印位附近可显著缓解；误报率靠 dry-run 攒数据校准。
3. **风格基线在首段（600s）完整落盘后才建立**——"开场 5 分钟"在分段录制约束下落地为首段落地后；第二段起每段比对。
4. **场景切换帧与固定间隔帧的时间点为近似值**（帧序×间隔/总时长均分），决策日志可辨识 scene 帧标记。
5. **OCR 未配置（ocr_bin 空）时广告文字/台标检测跳过**（记 skipped），模板匹配照常——不装 tesseract 不影响其他关卡。
6. **fail-closed**：enforce 下任一关卡工具崩溃/超时（默认 120s）一律不发；dry-run 记 `would-fail-closed` 不拦。
7. **关卡 3** 接现有 LLM 评分：等分切片（无 LLM）Score=0 走 `unscored` 只标注不拦（`reject_unscored: true` 可改为拦）。
8. **帧目录** `data/gate/frames/` 供命中复核，暂未接入磁盘治理（量小：每片 ≤10 张 160~320 宽 jpg）。

## 验证与样例

- 全量 build/vet/test 绿：gate 包 16 个测试（pHash 稳定性、NCC 命中/纯色防御、四关判定、fail-closed、决策日志回填、crop 处置、ffmpeg 真链路集成）+ config/store/live 各 1-4 个新测试。
- dry-run 回放 3 场合成直播：干净场 pass、水印场命中 8 帧 would-reject、漂移场整场 would-manual；人工回填（入池=approved）写回同一条验证通过。样例见 `temp_server/体检报告样例.json`。

## 第 2 期（本版不做）

水印抹除；发布后平台数据回标（72h 完播/互动拉回 → 决策日志扩充训练字段）；命中帧接入磁盘治理；水印区域可视化框选。
