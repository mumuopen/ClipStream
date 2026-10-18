package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"chopperbot/internal/core"

	_ "modernc.org/sqlite"
)

// Store SQLite 持久化（modernc 纯 Go 驱动，无 cgo）
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 多进程并发安全（serve 与 run/status 可同时运行）：WAL + 忙等 5s
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	// 单写者足够（任务量小）；避免进程内锁竞争
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		kind TEXT, status TEXT, stage TEXT, progress INTEGER,
		input TEXT, params TEXT, result TEXT, error TEXT, log TEXT,
		dir TEXT, created_at INTEGER, updated_at INTEGER
	)`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS watches (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		platform TEXT NOT NULL DEFAULT 'bilibili',
		url TEXT NOT NULL,
		room_id TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'idle',
		last_error TEXT NOT NULL DEFAULT '',
		live_start INTEGER NOT NULL DEFAULT 0,
		seg_dir TEXT NOT NULL DEFAULT '',
		seg_count INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER
	)`); err != nil {
		return err
	}
	// 弹幕密度桶（M4.2：5s 一桶，主键防重复累加）
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS barrage_buckets (
		room_id TEXT NOT NULL,
		bucket_ts INTEGER NOT NULL,
		count INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (room_id, bucket_ts)
	)`)
	if err != nil {
		return err
	}
	// 多账号（M5：矩阵账号与人设标签路由）
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		platform TEXT NOT NULL,      -- bilibili | douyin | xiaohongshu ...
		name TEXT NOT NULL,          -- 账号名
		labels TEXT NOT NULL DEFAULT '', -- 人设标签（逗号分隔）
		status TEXT NOT NULL DEFAULT 'ok', -- ok | login_expired
		priority INTEGER NOT NULL DEFAULT 0, -- 发布优先级（大优先）
		created_at INTEGER
	)`); err != nil {
		return err
	}
	// 发布调度（M5：定时错峰占位）
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schedules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		time TEXT NOT NULL,          -- HH:MM 每日触发
		platforms TEXT NOT NULL DEFAULT '',  -- 目标平台（逗号分隔，空=全部）
		account_ids TEXT NOT NULL DEFAULT '', -- 账号筛选（逗号分隔，空=按路由）
		labels TEXT NOT NULL DEFAULT '',  -- 内容标签筛选（逗号分隔）
		enabled INTEGER NOT NULL DEFAULT 1,
		last_run INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER
	)`); err != nil {
		return err
	}
	// 内容池（M5：审核通过待发布的切片）
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS content_pool (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_job TEXT NOT NULL,    -- 源剪辑任务 ID
		clip TEXT NOT NULL,          -- 切片相对路径 clips/clip_01.mp4
		cover TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL,
		desc TEXT NOT NULL DEFAULT '',
		tags TEXT NOT NULL DEFAULT '',       -- 内容标签（逗号分隔，用于账号路由）
		platforms TEXT NOT NULL DEFAULT '',  -- 目标平台（逗号分隔，空=全部）
		status TEXT NOT NULL DEFAULT 'pending', -- pending | dispatched | failed
		created_at INTEGER
	)`)
	if err != nil {
		return err
	}
	// 发布失败计数（MarkPoolItemRetry：连续失败 ≥3 次置 failed 终态）。
	// 幂等升级：老库补列，已存在则忽略
	if _, err := s.db.Exec(`ALTER TABLE content_pool ADD COLUMN fails INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// 节流三闸/调度恢复按 kind+created_at 扫 jobs：万级任务后泵 tick 查询
	// 会拖慢，索引兜底（幂等）
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_kind_created ON jobs(kind, created_at)`); err != nil {
		return err
	}
	return nil
}

// ── 直播关注（watches）──

// Watch 关注的直播间
type Watch struct {
	ID        int64  `json:"id"`
	Platform  string `json:"platform"`
	URL       string `json:"url"`
	RoomID    string `json:"room_id"`
	Title     string `json:"title"`
	Status    string `json:"status"` // idle | recording | error
	LastError string `json:"last_error"`
	LiveStart int64  `json:"live_start"`
	SegDir    string `json:"seg_dir"`
	SegCount  int    `json:"seg_count"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) AddWatch(w *Watch) (int64, error) {
	if w.Platform == "" {
		w.Platform = "bilibili"
	}
	if w.Status == "" {
		w.Status = "idle"
	}
	w.CreatedAt = time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO watches (platform,url,room_id,title,status,last_error,live_start,seg_dir,seg_count,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.Platform, w.URL, w.RoomID, w.Title, w.Status, w.LastError, w.LiveStart, w.SegDir, w.SegCount, w.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListWatches() ([]*Watch, error) {
	rows, err := s.db.Query(`SELECT id,platform,url,room_id,title,status,last_error,live_start,seg_dir,seg_count,created_at FROM watches ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Watch
	for rows.Next() {
		var w Watch
		if err := rows.Scan(&w.ID, &w.Platform, &w.URL, &w.RoomID, &w.Title, &w.Status, &w.LastError, &w.LiveStart, &w.SegDir, &w.SegCount, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}

func (s *Store) UpdateWatch(w *Watch) error {
	_, err := s.db.Exec(`UPDATE watches SET title=?,status=?,last_error=?,live_start=?,seg_dir=?,seg_count=? WHERE id=?`,
		w.Title, w.Status, w.LastError, w.LiveStart, w.SegDir, w.SegCount, w.ID)
	return err
}

func (s *Store) DeleteWatch(id int64) error {
	_, err := s.db.Exec(`DELETE FROM watches WHERE id=?`, id)
	return err
}

// ── 弹幕密度桶（M4.2）──

// BarrageBucket 弹幕密度桶：TS 桶起点 unix 秒；Count 桶内弹幕数
type BarrageBucket struct {
	TS    int64 `json:"ts"`
	Count int   `json:"count"`
}

// AddBarrage 累加一个密度桶（同桶跨会话/重连累加）
func (s *Store) AddBarrage(roomID string, bucketTS int64, count int) error {
	_, err := s.db.Exec(`INSERT INTO barrage_buckets (room_id,bucket_ts,count) VALUES (?,?,?)
		ON CONFLICT(room_id,bucket_ts) DO UPDATE SET count = count + excluded.count`,
		roomID, bucketTS, count)
	return err
}

// ListBarrage 读取 since（unix 秒）之后的密度桶，按时间升序
func (s *Store) ListBarrage(roomID string, since int64) ([]BarrageBucket, error) {
	rows, err := s.db.Query(`SELECT bucket_ts,count FROM barrage_buckets
		WHERE room_id=? AND bucket_ts>=? ORDER BY bucket_ts`, roomID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BarrageBucket
	for rows.Next() {
		var b BarrageBucket
		if err := rows.Scan(&b.TS, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// WatchByRoomID 按房间号查关注（录制守护进程 endSession 回写状态用）
func (s *Store) WatchByRoomID(roomID string) (*Watch, error) {
	row := s.db.QueryRow(`SELECT id,platform,url,room_id,title,status,last_error,live_start,seg_dir,seg_count,created_at
		FROM watches WHERE room_id=?`, roomID)
	return scanWatch(row)
}

func scanWatch(r scanner) (*Watch, error) {
	var w Watch
	err := r.Scan(&w.ID, &w.Platform, &w.URL, &w.RoomID, &w.Title, &w.Status,
		&w.LastError, &w.LiveStart, &w.SegDir, &w.SegCount, &w.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateJob(j *core.Job) error {
	j.CreatedAt = time.Now().Unix()
	j.UpdatedAt = j.CreatedAt
	_, err := s.db.Exec(`INSERT INTO jobs
		(id,kind,status,stage,progress,input,params,result,error,log,dir,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Kind, string(j.Status), j.Stage, j.Progress, j.Input,
		j.Params, j.Result, j.Error, j.Log, j.Dir, j.CreatedAt, j.UpdatedAt)
	return err
}

func (s *Store) UpdateJob(j *core.Job) error {
	j.UpdatedAt = time.Now().Unix()
	_, err := s.db.Exec(`UPDATE jobs SET
		status=?, stage=?, progress=?, params=?, result=?, error=?, log=?, updated_at=?
		WHERE id=?`,
		string(j.Status), j.Stage, j.Progress, j.Params, j.Result, j.Error, j.Log, j.UpdatedAt, j.ID)
	return err
}

func (s *Store) GetJob(id string) (*core.Job, error) {
	row := s.db.QueryRow(`SELECT id,kind,status,stage,progress,input,params,result,error,log,dir,created_at,updated_at
		FROM jobs WHERE id=?`, id)
	return scanJob(row)
}

func (s *Store) ListJobs(limit int) ([]*core.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,kind,status,stage,progress,input,params,result,error,log,dir,created_at,updated_at
		FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*core.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ResetInterrupted 服务重启后，把中断中的任务复位为排队（重新入队执行）
// 仅 running（真被中断）记入提示；queued 从未执行，不应误标
func (s *Store) ResetInterrupted() error {
	_, err := s.db.Exec(`UPDATE jobs SET status='queued',
		error=CASE WHEN status='running' THEN '服务重启，任务重新入队' ELSE '' END
		WHERE status IN ('queued','running')`)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanJob(r scanner) (*core.Job, error) {
	var j core.Job
	var status string
	err := r.Scan(&j.ID, &j.Kind, &status, &j.Stage, &j.Progress, &j.Input,
		&j.Params, &j.Result, &j.Error, &j.Log, &j.Dir, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	j.Status = core.JobStatus(status)
	return &j, nil
}

func (s *Store) DeleteJob(id string) error {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("任务不存在: %s", id)
	}
	return nil
}

// ── 多账号（M5 矩阵）──

// Account 平台发布账号
// LoggedIn 由 server 层根据磁盘 cookie 实时计算（磁盘即真相），不落库
// Status 仅在发布失败时回写 login_expired（routeAccount 过滤），扫码登录成功后恢复 ok
type Account struct {
	ID       int64  `json:"id"`
	Platform string `json:"platform"` // bilibili | douyin
	Name     string `json:"name"`     // 发布账号名 = 登录别名（data/accounts/<platform>-<name>）
	Labels   string `json:"labels"`  // 内容标签（逗号分隔，定时发布时匹配选题）
	Status   string `json:"status"`  // ok | login_expired
	Priority int    `json:"priority"` // 优先级：同标签多账号时大的先用
	LoggedIn bool   `json:"logged_in"` // 机上体：登录态文件是否存在
}

func (s *Store) ListAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT id,platform,name,labels,status,priority FROM accounts ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Platform, &a.Name, &a.Labels, &a.Status, &a.Priority); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *Store) CreateAccount(a *Account) (int64, error) {
	if a.Status == "" {
		a.Status = "ok"
	}
	res, err := s.db.Exec(`INSERT INTO accounts (platform,name,labels,status,priority,created_at) VALUES (?,?,?,?,?,?)`,
		a.Platform, a.Name, a.Labels, a.Status, a.Priority, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

// AccountByName 按平台+账号名查账号（无则 nil）
func (s *Store) AccountByName(platform, name string) (*Account, error) {
	rows, err := s.db.Query(`SELECT id,platform,name,labels,status,priority FROM accounts WHERE platform=? AND name=?`, platform, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Platform, &a.Name, &a.Labels, &a.Status, &a.Priority); err != nil {
			return nil, err
		}
		return &a, nil
	}
	return nil, rows.Err()
}

// SetAccountStatus 回写账号状态：发布失败（登录态缺失）标 login_expired，扫码登录成功恢复 ok
func (s *Store) SetAccountStatus(platform, name, status string) error {
	_, err := s.db.Exec(`UPDATE accounts SET status=? WHERE platform=? AND name=?`, status, platform, name)
	return err
}

// ── 发布调度（M5 错峰占位）──

// Schedule 定时发布调度（每日 HH:MM）
type Schedule struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Time       string `json:"time"` // HH:MM
	Platforms  string `json:"platforms"`
	AccountIDs string `json:"account_ids"`
	Labels     string `json:"labels"`
	Enabled    bool   `json:"enabled"`
	LastRun    int64  `json:"last_run"`
	CreatedAt  int64  `json:"created_at"`
}

func (s *Store) ListSchedules() ([]*Schedule, error) {
	rows, err := s.db.Query(`SELECT id,name,time,platforms,account_ids,labels,enabled,last_run,created_at FROM schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Schedule
	for rows.Next() {
		var sc Schedule
		var en int
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.Time, &sc.Platforms, &sc.AccountIDs, &sc.Labels, &en, &sc.LastRun, &sc.CreatedAt); err != nil {
			return nil, err
		}
		sc.Enabled = en == 1
		out = append(out, &sc)
	}
	return out, rows.Err()
}

func (s *Store) CreateSchedule(sc *Schedule) (int64, error) {
	en := 1
	if !sc.Enabled {
		en = 0
	}
	res, err := s.db.Exec(`INSERT INTO schedules (name,time,platforms,account_ids,labels,enabled,created_at) VALUES (?,?,?,?,?,?,?)`,
		sc.Name, sc.Time, sc.Platforms, sc.AccountIDs, sc.Labels, en, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateScheduleLastRun(id int64) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_run=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

// EnableSchedule 启/停定时发布计划
func (s *Store) EnableSchedule(id int64, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	_, err := s.db.Exec(`UPDATE schedules SET enabled=? WHERE id=?`, en, id)
	return err
}

func (s *Store) DeleteSchedule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE id=?`, id)
	return err
}

// ── 内容池（M5：审核通过待发布）──

// PoolItem 内容池条目（待发布切片）
type PoolItem struct {
	ID        int64  `json:"id"`
	SourceJob string `json:"source_job"`
	Clip      string `json:"clip"`
	Cover     string `json:"cover"`
	Title     string `json:"title"`
	Desc      string `json:"desc"`
	Tags      string `json:"tags"`      // 内容标签（逗号分隔）
	Platforms string `json:"platforms"` // 目标平台（逗号分隔，空=全部）
	Status    string `json:"status"`    // pending | dispatched | failed
	Fails     int    `json:"fails"`     // 连续发布失败次数（≥3 终态退池；UI 重试角标）
	CreatedAt int64  `json:"created_at"`
}

// ErrAlreadyInPool 同一切片已有待发条目（入池查重：审核台双击/重复审核防护）
var ErrAlreadyInPool = errors.New("该切片已在待发池中，请勿重复入池")

// AddPoolItem 切片入池（审核通过）。同 (source_job, clip) 已有 pending 条目时
// 拒绝（ErrAlreadyInPool）：旧版无查重，双击入池即重复发布同一视频
func (s *Store) AddPoolItem(it *PoolItem) (int64, error) {
	if it.Status == "" {
		it.Status = "pending"
	}
	var existing int64
	if err := s.db.QueryRow(`SELECT id FROM content_pool WHERE source_job=? AND clip=? AND status='pending' LIMIT 1`,
		it.SourceJob, it.Clip).Scan(&existing); err == nil {
		return existing, ErrAlreadyInPool
	}
	res, err := s.db.Exec(`INSERT INTO content_pool (source_job,clip,cover,title,desc,tags,platforms,status,created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		it.SourceJob, it.Clip, it.Cover, it.Title, it.Desc, it.Tags, it.Platforms, it.Status, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListPoolItems 列内容池（默认只看待发；含 fails 供 UI 重试角标与排障）
func (s *Store) ListPoolItems(all bool) ([]*PoolItem, error) {
	q := `SELECT id,source_job,clip,cover,title,desc,tags,platforms,status,fails,created_at FROM content_pool`
	if !all {
		q += ` WHERE status='pending'`
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PoolItem
	for rows.Next() {
		var it PoolItem
		if err := rows.Scan(&it.ID, &it.SourceJob, &it.Clip, &it.Cover, &it.Title, &it.Desc, &it.Tags, &it.Platforms, &it.Status, &it.Fails, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

// SetPoolItemStatus 标记池条目状态（dispatched/failed）
func (s *Store) SetPoolItemStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE content_pool SET status=? WHERE id=?`, status, id)
	return err
}

// TrimPoolItemPlatforms 多平台部分成功后的重发范围收缩：从条目 platforms
// 剔除已成功平台（防回池重派时同一平台重复投稿）。返回剔除后剩余平台串；
// 剩空返回 ""（调用方应停止回池——该条目已全部发出）。
func (s *Store) TrimPoolItemPlatforms(id int64, succeeded []string) (string, error) {	var platforms string
	if err := s.db.QueryRow(`SELECT platforms FROM content_pool WHERE id=?`, id).Scan(&platforms); err != nil {
		return "", err
	}
	remove := map[string]bool{}
	for _, p := range succeeded {
		remove[p] = true
	}
	var left []string
	for _, p := range strings.Split(platforms, ",") {
		if p = strings.TrimSpace(p); p != "" && !remove[p] {
			left = append(left, p)
		}
	}
	out := strings.Join(left, ",")
	if _, err := s.db.Exec(`UPDATE content_pool SET platforms=? WHERE id=?`, out, id); err != nil {
		return "", err
	}
	return out, nil
}

// WithAccountLock 同账号派发互斥：把"查三闸 + 建任务 + 置 dispatched"包成
// 原子段，堵住手动发布与发布泵/调度同秒并发导致的双投稿竞态窗（serve 进程内
// 生效——两处调用方同进程；跨进程场景不存在：调度与泵都在 serve）
func (s *Store) WithAccountLock(name string, fn func()) {
	if name == "" {
		fn()
		return
	}
	v, _ := acctMu.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	fn()
}

var acctMu sync.Map

// MarkPoolItemRetry 发布失败回池（单条 SQL 原子化：多步 UPDATE/SELECT/UPDATE
// 中途崩溃会出现"计数已加但状态未回"的卡 dispatched 条目）。连续失败 ≥ maxFails
// 次置 failed 终态（返回 final=true，泵/调度不再自动派发，UI 红色"已退池"等
// 人工处置）；否则回 pending 由发布泵按间隔冷却重试——防单个坏条目反复撞墙
// 吃光当日配额。成功路径不调用本方法（fails 不清零：终态后需人工处置重置）
func (s *Store) MarkPoolItemRetry(id int64, maxFails int) (final bool, err error) {
	res, err := s.db.Exec(`UPDATE content_pool
		SET fails = fails + 1,
		    status = CASE WHEN fails + 1 >= ? THEN 'failed' ELSE 'pending' END
		WHERE id=?`, maxFails, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, fmt.Errorf("池条目 %d 不存在", id)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM content_pool WHERE id=?`, id).Scan(&status); err != nil {
		return false, err
	}
	return status == "failed", nil
}

// DeletePoolItem 删除池条目
func (s *Store) DeletePoolItem(id int64) error {
	_, err := s.db.Exec(`DELETE FROM content_pool WHERE id=?`, id)
	return err
}

// ── 发布节流（防风控：B站高频投稿限流/封号）──

// acctInputPattern 生成 jobs.input 的 LIKE 匹配模式：发布任务 input JSON
// 紧凑序列化含 "account":"<name>"。账号名允许下划线（bili_395147691），
// LIKE 里 _ 是单字符通配——必须 ESCAPE 消歧，否则 biliX... 会误匹配
func acctInputPattern(account string) string {
	const esc = `\`
	r := strings.NewReplacer(esc, esc+esc, "%", esc+"%", "_", esc+"_")
	return `%"account":"` + r.Replace(account) + `"%`
}

// dryRunFilter 排除演练任务：dry-run 不真投稿（B站零网络接触），计入间隔/
// 每日闸只会白白锁死配额。手动与自动路径序列化 dry_run 布尔字段，负向匹配可靠
const dryRunFilter = ` AND input NOT LIKE '%"dry_run":true%'`

// LastPublishAt 最近一次发布任务的派发时刻（Unix 秒）。account 为空查全局。
// 保守口径：不含状态过滤——失败的投稿尝试同样是风控行为特征；排除 dry-run
func (s *Store) LastPublishAt(account string) (int64, error) {
	var last sql.NullInt64
	q := `SELECT MAX(created_at) FROM jobs WHERE kind='publish'` + dryRunFilter
	var args []any
	if account != "" {
		q += ` AND input LIKE ? ESCAPE '\'`
		args = append(args, acctInputPattern(account))
	}
	if err := s.db.QueryRow(q, args...).Scan(&last); err != nil {
		return 0, err
	}
	return last.Int64, nil
}

// CountPublishSince 某账号自 since 起的发布任务数（account 为空查全局），
// 供每日上限判定；排除 dry-run（同 LastPublishAt）
func (s *Store) CountPublishSince(account string, since int64) (int, error) {
	q := `SELECT COUNT(*) FROM jobs WHERE kind='publish' AND created_at >= ?` + dryRunFilter
	var args []any
	args = append(args, since)
	if account != "" {
		q += ` AND input LIKE ? ESCAPE '\'`
		args = append(args, acctInputPattern(account))
	}
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
