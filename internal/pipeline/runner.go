package pipeline

import (
	"context"
	"sync"

	"chopperbot/internal/core"
)

// Stage 管线中的一个阶段
type Stage struct {
	Name string
	Run  func(ctx context.Context, job *core.Job, env *Env) error
}

// Runner 任务执行器：从队列取 Job，按 Kind 走对应管线
type Runner struct {
	env    *Env
	stages map[string][]Stage
	sem    chan struct{} // 并发上限
	mu     sync.Mutex
	active map[string]bool
}

func NewRunner(env *Env) *Runner {
	return &Runner{
		env:    env,
		stages: map[string][]Stage{"clips": clipStages()},
		sem:    make(chan struct{}, 2), // 最多 2 个任务并行
		active: map[string]bool{},
	}
}

// Enqueue 异步入队执行
func (r *Runner) Enqueue(id string) {
	go r.run(id)
}

func (r *Runner) Active(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[id]
}

func (r *Runner) run(id string) {
	r.sem <- struct{}{}
	defer func() { <-r.sem }()

	job, err := r.env.Store.GetJob(id)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.active[id] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
	}()

	ctx := context.Background()
	job.Status = core.StatusRunning
	job.Error = ""
	_ = r.env.Store.UpdateJob(job)

	stages := r.stages[job.Kind]
	if len(stages) == 0 {
		stages = r.stages["clips"]
	}

	for i, st := range stages {
		job.Stage = st.Name
		job.Progress = i * 100 / len(stages)
		_ = r.env.Store.UpdateJob(job)
		r.env.log(job, "▶ 阶段开始：%s", st.Name)
		r.env.publishStatus(job)

		if err := st.Run(ctx, job, r.env); err != nil {
			if ctx.Err() != nil {
				job.Status = core.StatusCanceled
				job.Error = "已取消"
			} else {
				job.Status = core.StatusFailed
				job.Error = err.Error()
			}
			r.env.log(job, "✗ 阶段失败：%v", err)
			_ = r.env.Store.UpdateJob(job)
			r.env.publishStatus(job)
			return
		}
		job.Progress = (i + 1) * 100 / len(stages)
		_ = r.env.Store.UpdateJob(job)
		r.env.log(job, "✔ 阶段完成：%s", st.Name)
		r.env.publishStatus(job)
	}

	// 终态由 finalize 阶段设置（review/done），兜底保护
	if !job.Status.Terminal() && job.Status != core.StatusReview {
		job.Status = core.StatusDone
	}
	job.Progress = 100
	_ = r.env.Store.UpdateJob(job)
	r.env.publishStatus(job)
}