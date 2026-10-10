package pipeline

import (
	"encoding/json"
	"fmt"

	"chopperbot/internal/config"
	"chopperbot/internal/core"
	"chopperbot/internal/engine"
	"chopperbot/internal/notify"
	"chopperbot/internal/store"
)

// Env 管线运行环境：引擎 + 存储 + 事件总线

type Env struct {
	Cfg    *config.Config
	Store  *store.Store
	YTDLP  *engine.YTDLP
	FFMPEG *engine.FFMPEG
	ASR    engine.ASR
	LLM    *engine.LLMClient
	Bus    *core.Bus
	Notify *notify.Pushme
}

func (env *Env) log(j *core.Job, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	j.AppendLog(line)
	b, _ := json.Marshal(map[string]string{"type": "log", "stage": j.Stage, "msg": line})
	env.Bus.Publish(j.ID, string(b))
	_ = env.Store.UpdateJob(j)
}

func (env *Env) publishStatus(j *core.Job) {
	b, _ := json.Marshal(map[string]any{"type": "job", "job": j})
	env.Bus.Publish(j.ID, string(b))
}

// notify 发推送（未配置时安静返回，失败不影响主流程）
func (env *Env) notify(title, content string) {
	if env.Notify != nil {
		env.Notify.Notify(title, content)
	}
}
