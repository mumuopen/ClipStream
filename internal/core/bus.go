package core

import "sync"

// Bus 任务事件总线（内存版）：供 SSE 订阅任务的实时日志/状态
type Bus struct {
	mu   sync.Mutex
	subs map[string][]chan string
}

func NewBus() *Bus {
	return &Bus{subs: map[string][]chan string{}}
}

// Publish 向订阅该任务的通道推送事件（不阻塞）
func (b *Bus) Publish(jobID, data string) {
	b.mu.Lock()
	chs := make([]chan string, len(b.subs[jobID]))
	copy(chs, b.subs[jobID])
	b.mu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- data:
		default:
		}
	}
}

// Subscribe 订阅某个任务的事件流
func (b *Bus) Subscribe(jobID string) chan string {
	ch := make(chan string, 128)
	b.mu.Lock()
	b.subs[jobID] = append(b.subs[jobID], ch)
	b.mu.Unlock()
	return ch
}

func (b *Bus) Unsubscribe(jobID string, target chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chs := b.subs[jobID]
	for i, ch := range chs {
		if ch == target {
			b.subs[jobID] = append(chs[:i], chs[i+1:]...)
			break
		}
	}
}
