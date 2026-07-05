package main

import (
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type WrapperInstance struct {
	Id            string    `json:"id"`
	Region        string    `json:"region"`
	DecryptPort   int       `json:"-"`
	M3U8Port      int       `json:"-"`
	M3U8Health    int32     `json:"-"`
	LastM3U8Error time.Time `json:"-"`
	NoRestart     bool      `json:"-"`
	Cmd           *exec.Cmd `json:"-"`

	// 热路径字段：选择阶段每 sample 高频读取，改为原子访问以避免 per-instance 锁争抢。
	client atomic.Pointer[DecryptClient]
	ready  atomic.Bool

	CrashTimes []time.Time `json:"-"`
	// mu 仅保护 CrashTimes / M3U8Health / LastM3U8Error 等复合状态。
	mu sync.Mutex
}

func (w *WrapperInstance) Lock() {
	w.mu.Lock()
}

func (w *WrapperInstance) Unlock() {
	w.mu.Unlock()
}

// GetClient / SetClient / IsReady / SetReady 提供热字段的无锁原子访问。
func (w *WrapperInstance) GetClient() *DecryptClient {
	return w.client.Load()
}

func (w *WrapperInstance) SetClient(c *DecryptClient) {
	w.client.Store(c)
}

func (w *WrapperInstance) IsReady() bool {
	return w.ready.Load()
}

func (w *WrapperInstance) SetReady(v bool) {
	w.ready.Store(v)
}

// Ensure the score reflects crashes within the last 15 minutes.
// Max penalty is applied for recent crashes.
// If there are 3 or more crashes within the last 15 minutes, it is considered unhealthy.
func (w *WrapperInstance) CalculateHealthPenalty() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	penalty := 0
	for _, t := range w.CrashTimes {
		age := now.Sub(t)
		if age <= 15*time.Minute {
			// e.g. 200 points scaled by how recent it is (0 minutes = 200, 15 minutes = 0)
			decay := 1.0 - (age.Seconds() / (15 * 60))
			if decay < 0 {
				decay = 0
			}
			penalty += int(200 * decay)
		}
	}
	return penalty
}

func (w *WrapperInstance) IsUnhealthy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	recentCrashes := 0
	for _, t := range w.CrashTimes {
		if age := now.Sub(t); age >= 0 && age <= 15*time.Minute {
			recentCrashes++
		}
	}
	return recentCrashes >= 3
}

func (w *WrapperInstance) ReportM3U8Error() {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	// 2 seconds cooldown for concurrent penalty
	if now.Sub(w.LastM3U8Error) > 2*time.Second {
		w.M3U8Health -= 20
		if w.M3U8Health < 0 {
			w.M3U8Health = 0
		}
		w.LastM3U8Error = now
	}
}

func (w *WrapperInstance) ReportM3U8Success() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.M3U8Health = 100
}

func (w *WrapperInstance) RecoverM3U8Health(amount int32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.M3U8Health += amount
	if w.M3U8Health > 100 {
		w.M3U8Health = 100
	}
}

func (w *WrapperInstance) GetM3U8Health() int32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.M3U8Health
}
