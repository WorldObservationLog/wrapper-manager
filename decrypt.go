package main

import (
	"fmt"
)

var WMDispatcher *Dispatcher

type Dispatcher struct {
}

type Task struct {
	AdamId  string
	Key     string
	Payload []byte
	Result  chan *Result
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
	inst, err := GlobalManager.SelectInstance(task.AdamId)
	if err != nil {
		task.Result <- &Result{
			Success: false,
			Data:    task.Payload,
			Error:   err,
		}
		return
	}
	if inst == nil {
		task.Result <- &Result{
			Success: false,
			Data:    task.Payload,
			Error:   fmt.Errorf("no available instance"),
		}
		return
	}

	// Ensure client is ready
	if inst.Client == nil {
		task.Result <- &Result{
			Success: false,
			Data:    task.Payload,
			Error:   fmt.Errorf("instance client not ready"),
		}
		return
	}

	inst.Client.Process(task)
}
