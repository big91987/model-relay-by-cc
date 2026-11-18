package gateway

import (
	"sync"
	"time"
)

// limiter 实现按 Key 的 RPM 滑动窗口限流与在途请求数上限。
// 当前部署范围是单节点网关，状态保存在进程内存中；重启后窗口自然清零。
type limiter struct {
	mu       sync.Mutex
	windows  map[int64][]int64 // keyID -> 最近 60 秒内的请求时间戳（毫秒，升序）
	inflight map[int64]int
}

func newLimiter() *limiter {
	return &limiter{windows: map[int64][]int64{}, inflight: map[int64]int{}}
}

const rpmWindowMs = 60_000

// allowRPM 记录一次请求；超过限制时返回 false 及建议的重试等待秒数。
func (l *limiter) allowRPM(keyID int64, limit int) (ok bool, remaining int, retryAfterSec int) {
	if limit <= 0 {
		return true, -1, 0
	}
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[keyID]
	i := 0
	for i < len(w) && w[i] <= now-rpmWindowMs {
		i++
	}
	w = w[i:]
	if len(w) >= limit {
		l.windows[keyID] = w
		// 最早的、仍占用名额的那次请求滑出窗口后才有空位
		oldest := w[len(w)-limit]
		wait := int((oldest+rpmWindowMs-now)/1000) + 1
		return false, 0, wait
	}
	w = append(w, now)
	l.windows[keyID] = w
	return true, limit - len(w), 0
}

// acquire 占用一个在途名额；返回的 release 可重复调用，只生效一次。
func (l *limiter) acquire(keyID int64, max int) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if max > 0 && l.inflight[keyID] >= max {
		return nil, false
	}
	l.inflight[keyID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.inflight[keyID]--
			if l.inflight[keyID] <= 0 {
				delete(l.inflight, keyID)
			}
			l.mu.Unlock()
		})
	}, true
}

func (l *limiter) inflightOf(keyID int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[keyID]
}

func (l *limiter) gc() {
	now := time.Now().UnixMilli()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, w := range l.windows {
		if len(w) == 0 || w[len(w)-1] <= now-rpmWindowMs {
			delete(l.windows, k)
		}
	}
}
