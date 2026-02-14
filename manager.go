package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
)

var GlobalManager *InstanceManager

type InstanceManager struct {
	instances map[string]*WrapperInstance
	mu        sync.RWMutex
	shutdown  bool
}

func NewInstanceManager() *InstanceManager {
	return &InstanceManager{
		instances: make(map[string]*WrapperInstance),
		shutdown:  false,
	}
}

func (m *InstanceManager) Add(inst *WrapperInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[inst.Id] = inst
}

func (m *InstanceManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, id)
}

func (m *InstanceManager) Get(id string) *WrapperInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
}

func (m *InstanceManager) List() []*WrapperInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		list = append(list, inst)
	}
	return list
}

func (m *InstanceManager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.shutdown {
		return nil
	}
	list := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		list = append(list, inst)
	}
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return AtomicWriteFile("data/instances.json", data)
}

func AtomicWriteFile(filename string, data []byte) error {
	f, err := os.CreateTemp("data", "instances-*.json")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	closed := false

	defer func() {
		if !closed {
			f.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	closed = true

	return os.Rename(tmpName, filename)
}

func LoadInstance() *InstanceManager {
	manager := NewInstanceManager()
	if _, err := os.Stat("data/instances.json"); os.IsNotExist(err) {
		return manager
	}
	var instances []*WrapperInstance
	content, err := os.ReadFile("data/instances.json")
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(content, &instances)
	if err != nil {
		panic(err)
	}
	for _, inst := range instances {
		manager.instances[inst.Id] = inst
	}
	return manager
}

func (m *InstanceManager) SelectInstance(adamId string) (*WrapperInstance, error) {
	// Snapshot instances to minimize lock holding time
	m.mu.RLock()
	instancesSnapshot := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		instancesSnapshot = append(instancesSnapshot, inst)
	}
	m.mu.RUnlock()

	// 1. Stickiness check (fast, no network)
	for _, inst := range instancesSnapshot {
		if inst.Client != nil && inst.Client.GetLastAdamId() == adamId {
			return inst, nil
		}
	}

	// 2. Region availability check (parallel)
	type result struct {
		inst *WrapperInstance
		err  error
	}

	candidatesChan := make(chan result, len(instancesSnapshot))
	var wg sync.WaitGroup

	for _, inst := range instancesSnapshot {
		wg.Add(1)
		go func(instance *WrapperInstance) {
			defer wg.Done()
			available, err := checkAvailableOnRegion(adamId, instance.Region, false)
			if err != nil {
				// Log error but don't fail the whole selection?
				// Original logic returned error immediately.
				// Let's keep consistent: if any fails, we might miss it, but returning error from parallel is tricky.
				// We'll collect errors, but prioritize available instances.
				candidatesChan <- result{inst: nil, err: err}
				return
			}
			if available {
				candidatesChan <- result{inst: instance, err: nil}
			}
		}(inst)
	}

	go func() {
		wg.Wait()
		close(candidatesChan)
	}()

	var candidates []*WrapperInstance
	var lastErr error

	for res := range candidatesChan {
		if res.err != nil {
			lastErr = res.err
			continue
		}
		if res.inst != nil {
			if res.inst.Client != nil && res.inst.Client.GetLastAdamId() == "" {
				// Found an idle instance, return immediately?
				// In parallel execution, we might get this later than others.
				// Let's collect all and prioritize later, or return first idle found?
				// Returning first idle found is faster.
				return res.inst, nil
			}
			candidates = append(candidates, res.inst)
		}
	}

	if len(candidates) > 0 {
		return candidates[rand.Intn(len(candidates))], nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, nil
}

func (m *InstanceManager) SelectInstanceForLyrics(adamId string, language string) *WrapperInstance {
	token, err := GetToken()
	if err != nil {
		return nil
	}

	// Snapshot instances
	m.mu.RLock()
	instancesSnapshot := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		instancesSnapshot = append(instancesSnapshot, inst)
	}
	m.mu.RUnlock()

	candidatesChan := make(chan *WrapperInstance, len(instancesSnapshot))
	var wg sync.WaitGroup

	for _, inst := range instancesSnapshot {
		wg.Add(1)
		go func(instance *WrapperInstance) {
			defer wg.Done()
			musicToken, err := GetMusicToken(instance)
			if err != nil {
				return
			}
			if HasLyrics(adamId, instance.Region, language, token, musicToken) {
				candidatesChan <- instance
			}
		}(inst)
	}

	go func() {
		wg.Wait()
		close(candidatesChan)
	}()

	var candidates []*WrapperInstance
	for inst := range candidatesChan {
		candidates = append(candidates, inst)
	}

	if len(candidates) > 0 {
		return candidates[rand.Intn(len(candidates))]
	}
	return nil
}

func (m *InstanceManager) StopAll() {
	m.mu.Lock()
	m.shutdown = true
	// We save before killing to ensure current state is persisted
	// Since shutdown is true, subsequent Save calls (from wrapperDown) will be no-ops
	m.mu.Unlock() // Unlock to call explicit internal save logic if needed, but we already have strict logic.

	// Force save once before killing?
	// Actually Save() checks shutdown flag.
	// So we should save BEFORE setting shutdown=true?
	// But StopAll is called during shutdown.
	// Let's manually save.

	manualSave := func() {
		m.mu.RLock()
		defer m.mu.RUnlock()
		list := make([]*WrapperInstance, 0, len(m.instances))
		for _, inst := range m.instances {
			list = append(list, inst)
		}
		data, err := json.Marshal(list)
		if err != nil {
			fmt.Printf("failed to marshal instances: %v\n", err)
			return
		}
		err = os.WriteFile("data/instances.json", data, 0644)
		if err != nil {
			fmt.Printf("failed to save instances: %v\n", err)
		}
	}
	manualSave()

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, inst := range m.instances {
		if inst.Cmd != nil && inst.Cmd.Process != nil {
			fmt.Printf("Stopping wrapper %s\n", inst.Id)
			inst.Cmd.Process.Kill()
		}
	}
}
