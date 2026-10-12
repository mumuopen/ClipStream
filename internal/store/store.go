package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"chopperbot/internal/core"
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
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS watches (
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
