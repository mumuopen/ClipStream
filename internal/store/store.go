package store

import (
	"database/sql"
	"fmt"
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
	return err
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

// Account 平台矩阵账号
type Account struct {
	ID       int64  `json:"id"`
	Platform string `json:"platform"` // bilibili | douyin | xiaohongshu ...
	Name     string `json:"name"`
	Labels   string `json:"labels"`  // 人设标签（逗号分隔）
	Status   string `json:"status"`  // ok | login_expired
	Priority int    `json:"priority"` // 发布优先级
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
	CreatedAt int64  `json:"created_at"`
}

// AddPoolItem 切片入池（审核通过）
func (s *Store) AddPoolItem(it *PoolItem) (int64, error) {
	if it.Status == "" {
		it.Status = "pending"
	}
	res, err := s.db.Exec(`INSERT INTO content_pool (source_job,clip,cover,title,desc,tags,platforms,status,created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		it.SourceJob, it.Clip, it.Cover, it.Title, it.Desc, it.Tags, it.Platforms, it.Status, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListPoolItems 列内容池（默认只看待发布）
func (s *Store) ListPoolItems(all bool) ([]*PoolItem, error) {
	q := `SELECT id,source_job,clip,cover,title,desc,tags,platforms,status,created_at FROM content_pool`
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
		if err := rows.Scan(&it.ID, &it.SourceJob, &it.Clip, &it.Cover, &it.Title, &it.Desc, &it.Tags, &it.Platforms, &it.Status, &it.CreatedAt); err != nil {
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

// DeletePoolItem 删除池条目
func (s *Store) DeletePoolItem(id int64) error {
	_, err := s.db.Exec(`DELETE FROM content_pool WHERE id=?`, id)
	return err
}
