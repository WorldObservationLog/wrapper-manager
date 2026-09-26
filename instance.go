package main

import (
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Instances is the global registry of running (or starting) account instances.
var Instances []*WrapperInstance

// instancesMu guards Instances.
var instancesMu sync.RWMutex

// instanceConcurrency is the maximum number of in-flight requests forwarded
// to a single wrapper-lite instance. lite serialises its Apple playback/lease
// work internally; more than a couple of concurrent requests just pile up and
// can trip Apple-side throttling, so we cap per-instance concurrency and let
// the other instances absorb load.
const instanceConcurrency = 2

// FairPlay circuit breaker. When Apple refuses to process the content key
// (KDCanProcessCKC errors) an instance cannot serve /key; rather than evicting
// the account (its credentials and subscription may be fine), the instance is
// temporarily excluded from selection and retried after a cooldown.
//
// Circuit state lives outside WrapperInstance so that struct keeps its value
// semantics (it is copied while iterating persisted instances and serialised
// to JSON).
const (
	fairplayFailThreshold = 5
	fairplayCooldown      = 5 * time.Minute
)

type fairplayCircuitState struct {
	fails     int
	openUntil time.Time
}

var fairplayCircuits = struct {
	sync.Mutex
	m map[string]*fairplayCircuitState
}{m: make(map[string]*fairplayCircuitState)}

// circuitOpen reports whether the instance identified by id is temporarily
// excluded after repeated FairPlay failures. The circuit closes itself once
// the cooldown elapses, giving the instance another chance.
func circuitOpen(id string) bool {
	fairplayCircuits.Lock()
	defer fairplayCircuits.Unlock()
	st, ok := fairplayCircuits.m[id]
	if !ok || st.openUntil.IsZero() {
		return false
	}
	if time.Now().After(st.openUntil) {
		st.openUntil = time.Time{}
		st.fails = 0
		return false
	}
	return true
}

// recordFairplayFailure counts a FairPlay failure for the instance and opens
// the circuit once the threshold is reached. It returns true when the circuit
// just opened (so the caller can log it once).
func recordFairplayFailure(id string) bool {
	fairplayCircuits.Lock()
	defer fairplayCircuits.Unlock()
	st, ok := fairplayCircuits.m[id]
	if !ok {
		st = &fairplayCircuitState{}
		fairplayCircuits.m[id] = st
	}
	if !st.openUntil.IsZero() && time.Now().Before(st.openUntil) {
		return false // already open
	}
	st.fails++
	if st.fails >= fairplayFailThreshold {
		st.openUntil = time.Now().Add(fairplayCooldown)
		st.fails = 0
		return true
	}
	return false
}

// degradedCount returns how many registered instances are currently excluded
// from selection because their FairPlay circuit is open.
func degradedCount() int {
	n := 0
	for _, inst := range SnapshotInstances() {
		if circuitOpen(inst.Id) {
			n++
		}
	}
	return n
}

// resetFairplayCircuit clears any circuit state for an instance that is
// starting a fresh lifecycle.
func resetFairplayCircuit(id string) {
	fairplayCircuits.Lock()
	delete(fairplayCircuits.m, id)
	fairplayCircuits.Unlock()
}

type WrapperInstance struct {
	Id        string    `json:"id"`
	Region    string    `json:"region"`
	Port      int       `json:"port"`
	NoRestart bool      `json:"-"`
	Cmd       *exec.Cmd `json:"-"`

	// sem bounds concurrent requests forwarded to this instance. Lazily
	// initialised by acquireInstanceSlot.
	sem chan struct{} `json:"-"`
}

// ensureSem creates the per-instance semaphore on first use.
func (i *WrapperInstance) ensureSem() {
	if i.sem == nil {
		i.sem = make(chan struct{}, instanceConcurrency)
	}
}

// acquireInstanceSlot reserves a concurrency slot on the instance. It blocks
// until a slot is free or the deadline passes; on timeout it returns false so
// the caller can report a busy/overloaded response instead of piling up.
func (i *WrapperInstance) acquireInstanceSlot(timeout time.Duration) bool {
	i.ensureSem()
	select {
	case i.sem <- struct{}{}:
		return true
	case <-time.After(timeout):
		return false
	}
}

// releaseInstanceSlot frees a previously acquired slot.
func (i *WrapperInstance) releaseInstanceSlot() {
	if i.sem != nil {
		<-i.sem
	}
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
	// A fresh lifecycle for this instance id: reset the unhealthy-once guard
	// so a re-logged-in instance can be deactivated/removed again if needed,
	// and clear any FairPlay circuit left over from the previous lifecycle.
	unhealthyOnce.Delete(instance.Id)
	resetFairplayCircuit(instance.Id)
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
