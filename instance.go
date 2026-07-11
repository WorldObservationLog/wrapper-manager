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

	// mu 仅保护 M3U8Health / LastM3U8Error 等复合状态。
	// 崩溃历史 / 退避代数已移至 crash.go 的集中式 crashRecords（按账号 id），
	// 因为 WrapperInstance 每次重启都会重建，挂在对象上的状态会丢失。
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

// CalculateHealthPenalty / IsUnhealthy 委托给集中式崩溃跟踪器（crash.go）。
// 崩溃历史按账号 id 维护，跨进程重建保持一致。
func (w *WrapperInstance) CalculateHealthPenalty() int {
	return crashPenalty(w.Id)
}

func (w *WrapperInstance) IsUnhealthy() bool {
	return isCrashUnhealthy(w.Id)
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
