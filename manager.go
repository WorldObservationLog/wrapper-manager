package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"
)

var GlobalManager *InstanceManager

type InstanceManager struct {
	instances      map[string]*WrapperInstance
	failedRecords  map[string]map[string]time.Time // adamId -> instanceId -> latest failed time
	mu             sync.RWMutex
	failedRecordMu sync.RWMutex
	shutdown       bool
}

func NewInstanceManager() *InstanceManager {
	return &InstanceManager{
		instances:     make(map[string]*WrapperInstance),
		failedRecords: make(map[string]map[string]time.Time),
		shutdown:      false,
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

func (m *InstanceManager) ReportFailure(adamId string, instanceId string) {
	m.failedRecordMu.Lock()
	defer m.failedRecordMu.Unlock()
	if _, ok := m.failedRecords[adamId]; !ok {
		m.failedRecords[adamId] = make(map[string]time.Time)
	}
	m.failedRecords[adamId][instanceId] = time.Now()
}

func (m *InstanceManager) getFailedInstanceIds(adamId string, d time.Duration) map[string]bool {
	m.failedRecordMu.RLock()
	defer m.failedRecordMu.RUnlock()

	res := make(map[string]bool)
	records, ok := m.failedRecords[adamId]
	if !ok {
		return res
	}

	now := time.Now()
	for instId, failureTime := range records {
		if now.Sub(failureTime) < d {
			res[instId] = true
		}
	}
	return res
}

func (m *InstanceManager) SelectInstance(adamId string) (*WrapperInstance, error) {
	// Snapshot instances to minimize lock holding time
	m.mu.RLock()
	instancesSnapshot := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		instancesSnapshot = append(instancesSnapshot, inst)
	}
	m.mu.RUnlock()

	// 0. Get recently failed instances (e.g., failed within the last 10 minutes)
	failedInstanceIds := m.getFailedInstanceIds(adamId, 10*time.Minute)

	// Filter out instances that are not ready or are completely unhealthy
	var validSnapshot []*WrapperInstance
	for _, inst := range instancesSnapshot {
		inst.Lock()
		ready := inst.Ready
		client := inst.Client
		inst.Unlock()

		if ready && client != nil && !inst.IsUnhealthy() {
			validSnapshot = append(validSnapshot, inst)
		}
	}

	if len(validSnapshot) == 0 {
		return nil, fmt.Errorf("no healthy and ready instances available")
	}

	// Calculate scores for all valid candidates
	const MaxQueueThreshold = 3

	type candidate struct {
		inst  *WrapperInstance
		score int
	}

	var candidates []candidate

	for _, inst := range validSnapshot {
		client := inst.Client
		activeTasks := client.GetActiveTasks()
		targetAdamId := client.GetTargetAdamId()
		healthPenalty := inst.CalculateHealthPenalty()

		score := int(activeTasks) * 100
		score += healthPenalty

		if failedInstanceIds[inst.Id] {
			score += 50000 // Heavy penalty for recently failed this adamId, but not a hard block if it's the only one left
		}

		if targetAdamId == adamId {
			// Affinity (Bonus: 0 extra penalty)
			score += 0
		} else if targetAdamId == "" {
			// Idle (Spillover penalty)
			// Base idle penalty is MaxQueueThreshold * 100.
			// This means an idle instance will only be cheaper if affinity instances have >= MaxQueueThreshold active tasks.
			score += MaxQueueThreshold * 100
		} else {
			// Alien (occupied by another task)
			score += 999999 // Very heavy penalty
		}

		candidates = append(candidates, candidate{inst: inst, score: score})
	}

	// Find the minimum score
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no valid candidates after scoring")
	}

	bestCandidate := candidates[0]
	for _, c := range candidates[1:] {
		if c.score < bestCandidate.score {
			bestCandidate = c
		}
	}

	selectedInst := bestCandidate.inst

	// 1. Stickiness / Region fast check
	// We still need to ensure the region supports it if we are switching its context or if it's the first time
	// If it's already serving this adamId, region was already verified.
	if selectedInst.Client.GetTargetAdamId() != adamId {
		available, err := checkAvailableOnRegion(adamId, selectedInst.Region, false)
		if err != nil {
			return nil, err
		}
		if !available {
			// If the best candidate's region doesn't support it, we must fall back to parallel scanning
			// For simplicity and to avoid cascading failures on the fast path, we do a parallel scan of valid candidates

			type regResult struct {
				inst *WrapperInstance
				err  error
			}
			candidatesChan := make(chan regResult, len(validSnapshot))
			var wg sync.WaitGroup
			for _, inst := range validSnapshot {
				wg.Add(1)
				go func(instance *WrapperInstance) {
					defer wg.Done()
					avail, err := checkAvailableOnRegion(adamId, instance.Region, false)
					if err != nil {
						candidatesChan <- regResult{inst: nil, err: err}
						return
					}
					if avail {
						candidatesChan <- regResult{inst: instance, err: nil}
					}
				}(inst)
			}
			go func() {
				wg.Wait()
				close(candidatesChan)
			}()

			var availableCandidates []*WrapperInstance
			var lastErr error
			for res := range candidatesChan {
				if res.err != nil {
					lastErr = res.err
					continue
				}
				if res.inst != nil {
					availableCandidates = append(availableCandidates, res.inst)
				}
			}

			if len(availableCandidates) == 0 {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, nil // None available in region
			}

			// Rescore the filtered available instances
			bestCandidate = candidate{score: 2147483647} // Max int
			for _, inst := range availableCandidates {
				client := inst.Client
				active := client.GetActiveTasks()
				tId := client.GetTargetAdamId()
				hPenalty := inst.CalculateHealthPenalty()

				s := int(active)*100 + hPenalty
				if failedInstanceIds[inst.Id] {
					s += 50000
				}
				if tId == adamId {
					s += 0
				} else if tId == "" {
					s += MaxQueueThreshold * 100
				} else {
					s += 999999
				}

				if s < bestCandidate.score {
					bestCandidate = candidate{inst: inst, score: s}
				}
			}
			selectedInst = bestCandidate.inst
		}
	}

	// 3. Declare intention before returning to prevent concurrency storms
	selectedInst.Client.SetTargetAdamId(adamId)

	return selectedInst, nil
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
