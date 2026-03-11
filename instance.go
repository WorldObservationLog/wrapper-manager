package main

import (
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type WrapperInstance struct {
	Id                      string         `json:"id"`
	Region                  string         `json:"region"`
	DecryptPort             int            `json:"-"`
	M3U8Port                int            `json:"-"`
	M3U8ConsecutiveFailures atomic.Int32   `json:"-"`
	NoRestart               bool           `json:"-"`
	Cmd                     *exec.Cmd      `json:"-"`
	Client                  *DecryptClient `json:"-"`
	Ready                   bool           `json:"-"`
	CrashTimes              []time.Time    `json:"-"`
	mu                      sync.Mutex
}

func (w *WrapperInstance) Lock() {
	w.mu.Lock()
}

func (w *WrapperInstance) Unlock() {
	w.mu.Unlock()
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
