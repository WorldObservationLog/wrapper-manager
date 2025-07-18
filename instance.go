package main

import (
	"encoding/json"
	"os"
	"os/exec"
)

var Instances []*WrapperInstance

type WrapperInstance struct {
	Id          string    `json:"id"`
	Region      string    `json:"region"`
	DecryptPort int       `json:"-"`
	M3U8Port    int       `json:"-"`
	NoRestart   bool      `json:"-"`
	Cmd         *exec.Cmd `json:"-"`
}

func SaveInstances() {
	instances, err := json.Marshal(Instances)
	if err != nil {
		panic(err)
	}
	err = os.WriteFile("data/instances.json", instances, 0777)
	if err != nil {
		panic(err)
	}
}

func LoadInstance() []WrapperInstance {
	if _, err := os.Stat("data/instances.json"); os.IsNotExist(err) {
		return make([]WrapperInstance, 0)
	}
	var instances []WrapperInstance
	content, err := os.ReadFile("data/instances.json")
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(content, &instances)
	if err != nil {
		panic(err)
	}
	return instances
}

func InsertInstance(instance *WrapperInstance) {
	for _, existing := range Instances {
		if existing.Id == instance.Id {
			return
		}
	}
	Instances = append(Instances, instance)
}

func RemoveInstance(instance *WrapperInstance) {
	for i, existing := range Instances {
		if existing.Id == instance.Id {
			Instances = append(Instances[:i], Instances[i+1:]...)
			return
		}
	}
}

func GetInstance(id string) *WrapperInstance {
	for _, instance := range Instances {
		if instance.Id == id {
			return instance
		}
	}
	return &WrapperInstance{}
}
