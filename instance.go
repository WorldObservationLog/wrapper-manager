package main

import (
	"encoding/json"
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
		panic(err)
	}
	err = os.WriteFile("data/instances.json", data, 0777)
	if err != nil {
		panic(err)
	}
}

func LoadInstance() []WrapperInstance {
	if _, err := os.Stat("data/instances.json"); os.IsNotExist(err) {
		return make([]WrapperInstance, 0)
	}
	content, err := os.ReadFile("data/instances.json")
	if err != nil {
		panic(err)
	}
	var instances []WrapperInstance
	if err = json.Unmarshal(content, &instances); err != nil {
		panic(err)
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
