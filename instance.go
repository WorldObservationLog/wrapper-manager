package main

import (
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"sync"
)

// Instances is the global registry of running (or starting) account instances.
var Instances []*WrapperInstance

// instancesMu guards Instances.
var instancesMu sync.RWMutex

type WrapperInstance struct {
	Id        string    `json:"id"`
	Region    string    `json:"region"`
	Port      int       `json:"port"`
	NoRestart bool      `json:"-"`
	Cmd       *exec.Cmd `json:"-"`
}

func SaveInstances() {
	instancesMu.RLock()
	list := make([]WrapperInstance, 0, len(Instances))
	for _, inst := range Instances {
		if inst == nil {
			continue
		}
		list = append(list, WrapperInstance{
			Id:     inst.Id,
			Region: inst.Region,
			Port:   inst.Port,
		})
	}
	instancesMu.RUnlock()

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		log.Errorf("failed to marshal instances: %v", err)
		return
	}
	// Atomic write: temp file + rename avoids a truncated registry on crash.
	tmp := "data/instances.json.tmp"
	if err = os.WriteFile(tmp, data, 0777); err != nil {
		log.Errorf("failed to write instances.json: %v", err)
		return
	}
	if err = os.Rename(tmp, "data/instances.json"); err != nil {
		log.Errorf("failed to rename instances.json: %v", err)
		return
	}
}

func LoadInstance() []WrapperInstance {
	if _, err := os.Stat("data/instances.json"); os.IsNotExist(err) {
		return make([]WrapperInstance, 0)
	}
	content, err := os.ReadFile("data/instances.json")
	if err != nil {
		log.Warnf("failed to read instances.json: %v", err)
		return make([]WrapperInstance, 0)
	}
	var instances []WrapperInstance
	if err = json.Unmarshal(content, &instances); err != nil {
		// A corrupted/truncated registry (e.g. after a crash mid-write) must
		// not prevent startup: back it up and start with an empty registry.
		log.Warnf("instances.json is corrupted (%v); backing up and starting empty", err)
		_ = os.Rename("data/instances.json", "data/instances.json.corrupt")
		return make([]WrapperInstance, 0)
	}
	return instances
}

func InsertInstance(instance *WrapperInstance) {
	instancesMu.Lock()
	defer instancesMu.Unlock()
	for _, existing := range Instances {
		if existing != nil && existing.Id == instance.Id {
			return
		}
	}
	Instances = append(Instances, instance)
}

func RemoveInstance(instance *WrapperInstance) {
	instancesMu.Lock()
	defer instancesMu.Unlock()
	for i, existing := range Instances {
		if existing != nil && existing.Id == instance.Id {
			Instances = append(Instances[:i], Instances[i+1:]...)
			return
		}
	}
}

// SnapshotInstances returns a copy of the registry for read-mostly callers.
func SnapshotInstances() []*WrapperInstance {
	instancesMu.RLock()
	defer instancesMu.RUnlock()
	out := make([]*WrapperInstance, 0, len(Instances))
	for _, inst := range Instances {
		if inst == nil {
			continue
		}
		out = append(out, inst)
	}
	return out
}

// GetInstance returns the instance with the given id, or nil when absent.
func GetInstance(id string) *WrapperInstance {
	instancesMu.RLock()
	defer instancesMu.RUnlock()
	for _, instance := range Instances {
		if instance != nil && instance.Id == id {
			return instance
		}
	}
	return nil
}
