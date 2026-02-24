package main

import (
	"fmt"
	"time"
)

var WMDispatcher *Dispatcher

type Dispatcher struct {
}

type Task struct {
	AdamId      string
	Key         string
	SampleIndex int32
	Payload     []byte
	Result      chan *Result
}

type Result struct {
	Success bool
	Data    []byte
	Error   error
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{}
}

func (d *Dispatcher) AddInstance(inst *WrapperInstance) {
	// Deprecated: Instance management is handled by GlobalManager
}

func (d *Dispatcher) RemoveInstance(id string) {
	// Deprecated: Instance management is handled by GlobalManager
}

func (d *Dispatcher) Submit(task *Task) {
	const maxRetries = 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		inst, err := GlobalManager.SelectInstance(task.AdamId, task.Key)
		if err != nil {
			lastErr = err
			// It's possible there are no instances available *yet* (e.g. all restarting)
			// Give it a brief moment before retrying
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if inst == nil {
			lastErr = fmt.Errorf("no available instance")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Ensure client is ready
		if inst.Client == nil {
			lastErr = fmt.Errorf("instance client not ready")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		resultData, opErr := inst.Client.Process(task.AdamId, task.Key, task.Payload)
		if opErr == nil {
			// Success
			task.Result <- &Result{
				Success: true,
				Data:    resultData,
				Error:   nil,
			}
			return
		}

		// Process failed (e.g., node network broken).
		// The node is marked broken and its score is penalized.
		// Next iteration will automatically pick another healthy node.
		lastErr = opErr
		time.Sleep(100 * time.Millisecond)
	}

	// All retries exhausted
	task.Result <- &Result{
		Success: false,
		Data:    task.Payload,
		Error:   fmt.Errorf("task failed after %d retries: %v", maxRetries, lastErr),
	}
}
