package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

var GlobalManager *InstanceManager

// scoredCandidate 是实例选择打分的中间结果。
type scoredCandidate struct {
	inst  *WrapperInstance
	score int
}

type InstanceManager struct {
	instances      map[string]*WrapperInstance
	instancesCache atomic.Value                    // 存储 []*WrapperInstance 实现无锁读取
	failedRecords  map[string]map[string]time.Time // adamId -> instanceId -> latest failed time
	mu             sync.RWMutex
	failedRecordMu sync.RWMutex
	shutdown       bool
}

func NewInstanceManager() *InstanceManager {
	m := &InstanceManager{
		instances:     make(map[string]*WrapperInstance),
		failedRecords: make(map[string]map[string]time.Time),
		shutdown:      false,
	}
	m.instancesCache.Store(make([]*WrapperInstance, 0))
	return m
}

// updateCache 必须在持有 mu.Lock() 或单核初始化时调用
func (m *InstanceManager) updateCache() {
	list := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		list = append(list, inst)
	}
	m.instancesCache.Store(list)
}

func (m *InstanceManager) Add(inst *WrapperInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[inst.Id] = inst
	m.updateCache()
}

func (m *InstanceManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, id)
	m.updateCache()
}

func (m *InstanceManager) Get(id string) *WrapperInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[id]
}

func (m *InstanceManager) List() []*WrapperInstance {
	cached := m.instancesCache.Load()
	if cached == nil {
		return []*WrapperInstance{}
	}
	return cached.([]*WrapperInstance)
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
		inst.M3U8Health = 100
		manager.instances[inst.Id] = inst
	}
	manager.updateCache()
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

// CleanupFailedRecords 回收那些已长久过期的 M3U8 失败记录，防止长期运行内存泄漏
func (m *InstanceManager) CleanupFailedRecords(d time.Duration) {
	m.failedRecordMu.Lock()
	defer m.failedRecordMu.Unlock()

	now := time.Now()
	for adamId, records := range m.failedRecords {
		for instId, failureTime := range records {
			if now.Sub(failureTime) >= d {
				delete(records, instId)
			}
		}
		if len(records) == 0 {
			delete(m.failedRecords, adamId)
		}
	}
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

func (m *InstanceManager) SelectInstance(adamId string, key string) (*WrapperInstance, error) {
	// Snapshot instances via O(1) lock-free atomic read
	instancesSnapshot := m.instancesCache.Load().([]*WrapperInstance)

	// 1. FAST PATH: Stickiness check (affinity)
	// 本地直达查表：这是恢复 20MB/s 极速解密性能的核心。
	// 热字段 Ready/Client 已原子化，此处全程无锁，免去 per-instance 锁争抢。
	for _, inst := range instancesSnapshot {
		client := inst.GetClient()
		if inst.IsReady() && client != nil && !client.IsBroken() {
			if client.GetTargetAdamId() == adamId && client.GetTargetKey() == key {
				if !inst.IsUnhealthy() {
					// 命中缓存，直接放行，吞吐量提升至满带宽。
					return inst, nil
				}
			}
		}
	}

	// Filter out instances that are not ready or are completely unhealthy.
	// 过滤阶段一次性取出 client 指针随候选一起携带，后续打分复用同一引用，
	// 避免二次裸读 inst.Client 造成 data race / nil 解引用。
	type readyInstance struct {
		inst   *WrapperInstance
		client *DecryptClient
	}
	var validSnapshot []readyInstance
	for _, inst := range instancesSnapshot {
		client := inst.GetClient()
		if inst.IsReady() && client != nil && !client.IsBroken() && !inst.IsUnhealthy() {
			validSnapshot = append(validSnapshot, readyInstance{inst: inst, client: client})
		}
	}

	if len(validSnapshot) == 0 {
		return nil, fmt.Errorf("no healthy and ready instances available")
	}

	// Calculate scores for all valid candidates
	const MaxQueueThreshold = 3

	scoreFor := func(client *DecryptClient, inst *WrapperInstance) int {
		activeTasks := client.GetActiveTasks()
		targetAdamId := client.GetTargetAdamId()
		targetKey := client.GetTargetKey()
		healthPenalty := inst.CalculateHealthPenalty()

		score := int(activeTasks)*100 + healthPenalty

		if targetAdamId == adamId {
			if key == "" || targetKey == key {
				// Perfect affinity or M3U8 request (no specific key, just tie to adamId)
				score += 0
			} else {
				// Different key requires switchContext, penalize heavily to separate video/audio tracks
				score += 999999
			}
		} else if targetAdamId == "" {
			// Idle (Spillover penalty)
			// Base idle penalty is MaxQueueThreshold * 100.
			// This means an idle instance will only be cheaper if affinity instances have >= MaxQueueThreshold active tasks.
			score += MaxQueueThreshold * 100
		} else {
			// Alien (occupied by another task)
			score += 999999 // Very heavy penalty
		}
		return score
	}

	var candidates []scoredCandidate

	for _, ri := range validSnapshot {
		candidates = append(candidates, scoredCandidate{inst: ri.inst, score: scoreFor(ri.client, ri.inst)})
	}

	// Find the minimum score
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no valid candidates after scoring")
	}

	selectedInst := pickMinScore(candidates)

	// 1. Stickiness / Region fast check
	// We still need to ensure the region supports it if we are switching its context or if it's the first time
	// If it's already serving this adamId, region was already verified.
	selectedClient := selectedInst.GetClient()
	if selectedClient == nil {
		return nil, fmt.Errorf("selected instance client became unavailable")
	}
	if selectedClient.GetTargetAdamId() != adamId {
		available, err := regionCanServe(adamId, selectedInst.Region)
		if err != nil {
			return nil, err
		}
		if !available {
			// If the best candidate's region doesn't support it, we must fall back to parallel scanning
			// For simplicity and to avoid cascading failures on the fast path, we do a parallel scan of valid candidates

			type regResult struct {
				ri  readyInstance
				err error
			}
			candidatesChan := make(chan regResult, len(validSnapshot))
			var wg sync.WaitGroup
			for _, ri := range validSnapshot {
				wg.Add(1)
				go func(ri readyInstance) {
					defer wg.Done()
					avail, err := regionCanServe(adamId, ri.inst.Region)
					if err != nil {
						candidatesChan <- regResult{err: err}
						return
					}
					if avail {
						candidatesChan <- regResult{ri: ri}
					}
				}(ri)
			}
			go func() {
				wg.Wait()
				close(candidatesChan)
			}()

			var availableCandidates []readyInstance
			var lastErr error
			for res := range candidatesChan {
				if res.err != nil {
					lastErr = res.err
					continue
				}
				if res.ri.inst != nil {
					availableCandidates = append(availableCandidates, res.ri)
				}
			}

			if len(availableCandidates) == 0 {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, nil // None available in region
			}

			// Rescore the filtered available instances (reuse scoreFor + random tie-break)
			var rescored []scoredCandidate
			for _, ri := range availableCandidates {
				rescored = append(rescored, scoredCandidate{inst: ri.inst, score: scoreFor(ri.client, ri.inst)})
			}
			selectedInst = pickMinScore(rescored)
			selectedClient = selectedInst.GetClient()
			if selectedClient == nil {
				return nil, fmt.Errorf("selected instance client became unavailable")
			}
		}
	}

	// 3. Declare intention before returning to prevent concurrency storms
	selectedClient.SetTarget(adamId, key)

	return selectedInst, nil
}

// pickMinScore 返回最小分候选；并列时随机选取，避免突发的不同 track 因确定性
// tie-break 全部被分配到同一个实例，从而打散负载、提升聚合吞吐。
func pickMinScore(candidates []scoredCandidate) *WrapperInstance {
	minScore := candidates[0].score
	for _, c := range candidates[1:] {
		if c.score < minScore {
			minScore = c.score
		}
	}
	var tied []*WrapperInstance
	for _, c := range candidates {
		if c.score == minScore {
			tied = append(tied, c.inst)
		}
	}
	return tied[rand.Intn(len(tied))]
}

func (m *InstanceManager) SelectInstanceForLyrics(adamId string, language string) *WrapperInstance {
	token, err := GetToken()
	if err != nil {
		return nil
	}

	// Snapshot instances via lock-free cache
	instancesSnapshot := m.instancesCache.Load().([]*WrapperInstance)

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

func (m *InstanceManager) SelectM3U8Instance(adamId string) (*WrapperInstance, error) {
	instancesSnapshot := m.instancesCache.Load().([]*WrapperInstance)
	failedInstanceIds := m.getFailedInstanceIds(adamId, 10*time.Minute)

	var validSnapshot []*WrapperInstance
	for _, inst := range instancesSnapshot {
		client := inst.GetClient()
		if inst.IsReady() && client != nil && !client.IsBroken() && !inst.IsUnhealthy() {
			validSnapshot = append(validSnapshot, inst)
		}
	}

	if len(validSnapshot) == 0 {
		return nil, fmt.Errorf("no healthy and ready instances available")
	}

	type candidate struct {
		inst  *WrapperInstance
		score int
	}

	var bestCandidate candidate
	bestCandidate.score = 2147483647

	var wg sync.WaitGroup
	type regResult struct {
		inst *WrapperInstance
		err  error
	}
	candidatesChan := make(chan regResult, len(validSnapshot))

	for _, inst := range validSnapshot {
		wg.Add(1)
		go func(instance *WrapperInstance) {
			defer wg.Done()
			avail, err := regionCanServe(adamId, instance.Region)
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
		return nil, fmt.Errorf("no available instance on any region")
	}

	for _, inst := range availableCandidates {
		// Only consider basic health penalty and M3U8 failure penalty
		score := inst.CalculateHealthPenalty()
		if failedInstanceIds[inst.Id] {
			score += 5000000
		}

		if score < bestCandidate.score {
			bestCandidate = candidate{inst: inst, score: score}
		}
	}

	// Deliberately DO NOT call SetTarget! M3U8 is an independent TCP connection.
	return bestCandidate.inst, nil
}

func (m *InstanceManager) SelectWebInstance(adamId string) (*WrapperInstance, error) {
	instancesSnapshot := m.instancesCache.Load().([]*WrapperInstance)

	var validSnapshot []*WrapperInstance
	for _, inst := range instancesSnapshot {
		if inst.IsReady() {
			validSnapshot = append(validSnapshot, inst)
		}
	}

	if len(validSnapshot) == 0 {
		return nil, fmt.Errorf("no ready instances available")
	}

	var wg sync.WaitGroup
	type regResult struct {
		inst *WrapperInstance
		err  error
	}
	candidatesChan := make(chan regResult, len(validSnapshot))

	for _, inst := range validSnapshot {
		wg.Add(1)
		go func(instance *WrapperInstance) {
			defer wg.Done()
			avail, err := regionCanServe(adamId, instance.Region)
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
		return nil, fmt.Errorf("no available instance on any region")
	}

	return availableCandidates[rand.Intn(len(availableCandidates))], nil
}

func (m *InstanceManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 标记关停：后续来自 wrapperDown 的 Save() 将变为 no-op，
	// 以本次持久化的快照为最终状态，避免被进程退出回调覆盖。
	m.shutdown = true

	// 快照当前实例，原子持久化（复用 AtomicWriteFile，与 Save 保持一致）。
	list := make([]*WrapperInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		list = append(list, inst)
	}
	if data, err := json.Marshal(list); err != nil {
		log.Errorf("StopAll: failed to marshal instances: %v", err)
	} else if err := AtomicWriteFile("data/instances.json", data); err != nil {
		log.Errorf("StopAll: failed to save instances: %v", err)
	}

	// 杀掉所有 wrapper 进程。
	for _, inst := range list {
		if inst.Cmd != nil && inst.Cmd.Process != nil {
			log.Infof("Stopping wrapper %s", inst.Id)
			if err := inst.Cmd.Process.Kill(); err != nil {
				log.Errorf("StopAll: failed to kill wrapper %s: %v", inst.Id, err)
			}
		}
	}
}
