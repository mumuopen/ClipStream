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
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		kind TEXT, status TEXT, stage TEXT, progress INTEGER,
		input TEXT, params TEXT, result TEXT, error TEXT, log TEXT,
		dir TEXT, created_at INTEGER, updated_at INTEGER
	)`)
	return err
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
