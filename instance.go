package main

import (
	"os/exec"
	"sync"
	"time"
)

type WrapperInstance struct {
	Id          string         `json:"id"`
	Region      string         `json:"region"`
	DecryptPort int            `json:"-"`
	M3U8Port    int            `json:"-"`
	NoRestart   bool           `json:"-"`
	Cmd         *exec.Cmd      `json:"-"`
	Client      *DecryptClient `json:"-"`
	Ready       bool           `json:"-"`
	CrashTimes  []time.Time    `json:"-"`
	mu          sync.Mutex
}

func (w *WrapperInstance) Lock() {
	w.mu.Lock()
}

func (w *WrapperInstance) Unlock() {
	w.mu.Unlock()
}
