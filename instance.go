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

// instanceConcurrency is the maximum number of in-flight non-playback
// requests forwarded to a single wrapper-lite instance.
const instanceConcurrency = 2

// instancePlaybackConcurrency caps /key and /m3u8 at one in-flight request
// per instance: wrapper-lite serialises both paths behind a single
// g_playback_mutex, so admitting a second request only holds a manager slot
// until the upstream timeout fires.
const instancePlaybackConcurrency = 1

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

// Recovery policy for account-level failure signals.
//
// A single signal is not proof that an account is dead. Two cases seen in
// production:
//
//   - The developer-token fetch inside wrapper-lite can get permanently stuck:
//     the instance then reports no region and serves nothing, yet a restart
//     restores it completely.
//   - Apple sometimes reports a session or subscription problem that clears by
//     itself.
//
// So every signal first earns the instance a restart; only a signal that keeps
// coming back across restarts retires the account. This mirrors the FairPlay
// circuit above instead of acting on the first occurrence.
const (
	// devTokenRestartThreshold is the number of consecutive developer-token
	// failures that trigger a restart. Background refresh runs every 30
	// minutes, so this is roughly one stuck interval plus a margin.
	devTokenRestartThreshold = 3

	// recoveryRestartGap throttles restarts so one unhealthy instance cannot
	// spin.
	recoveryRestartGap = 90 * time.Second

	// maxRecoveryRestarts caps how often a single instance may be restarted
	// for health reasons before it is treated as genuinely dead.
	maxRecoveryRestarts = 6

	// recoveryBudgetReset restores the restart budget once an instance has
	// been quiet for this long, so a recovered instance is not punished for
	// failures that are already history.
	recoveryBudgetReset = 30 * time.Minute

	// sessionConfirmLimit and subscriptionConfirmLimit are how many times a
	// signal must be confirmed, across restarts, before the account is
	// retired.
	sessionConfirmLimit      = 3
	subscriptionConfirmLimit = 3
)

// instanceRecovery tracks restart activity for one instance. It lives outside
// WrapperInstance because that struct keeps value semantics (it is copied
// while iterating persisted instances and serialised to JSON).
type instanceRecovery struct {
	devTokenFails int
	sessionSeen   int
	subSeen       int
	restarts      int
	lastRestart   time.Time
}

var instanceRecoveries = struct {
	sync.Mutex
	m map[string]*instanceRecovery
}{m: make(map[string]*instanceRecovery)}

// recoveryStateLocked returns the tracker for id, creating it on first use.
// Callers must hold instanceRecoveries' lock.
func recoveryStateLocked(id string) *instanceRecovery {
	st, ok := instanceRecoveries.m[id]
	if !ok {
		st = &instanceRecovery{}
		instanceRecoveries.m[id] = st
	}
	// Quiet for a long time: forget earlier trouble, including the spent
	// restart budget.
	if time.Since(st.lastRestart) > recoveryBudgetReset && !st.lastRestart.IsZero() {
		*st = instanceRecovery{}
	}
	return st
}

// allowRestartLocked spends restart budget when a restart is due. Callers must
// hold instanceRecoveries' lock.
func allowRestartLocked(st *instanceRecovery) bool {
	if st.restarts >= maxRecoveryRestarts {
		return false
	}
	if time.Since(st.lastRestart) < recoveryRestartGap {
		return false
	}
	st.restarts++
	st.lastRestart = time.Now()
	return true
}

// noteDevTokenFailure counts a developer-token failure and reports whether the
// instance should be restarted. The counter resets whenever a restart is
// issued, so a stuck instance restarts once per stuck period rather than once
// per failing line.
func noteDevTokenFailure(id string) bool {
	instanceRecoveries.Lock()
	defer instanceRecoveries.Unlock()
	st := recoveryStateLocked(id)
	st.devTokenFails++
	if st.devTokenFails < devTokenRestartThreshold {
		return false
	}
	st.devTokenFails = 0
	return allowRestartLocked(st)
}

// noteFatalSignal records a session-invalid ("session") or subscription-dead
// ("subscription") signal. It reports whether the instance should be restarted
// (still worth another try) or retired (the signal survived its restarts).
func noteFatalSignal(id, kind string) (restart, retire bool) {
	instanceRecoveries.Lock()
	defer instanceRecoveries.Unlock()
	st := recoveryStateLocked(id)
	switch kind {
	case "session":
		st.sessionSeen++
		if st.sessionSeen >= sessionConfirmLimit {
			return false, true
		}
	case "subscription":
		st.subSeen++
		if st.subSeen >= subscriptionConfirmLimit {
			return false, true
		}
	}
	if allowRestartLocked(st) {
		return true, false
	}
	// Out of restart budget: the instance has failed often enough that
	// another restart is not worth it.
	if st.restarts >= maxRecoveryRestarts {
		return false, true
	}
	// Restarting is merely throttled; leave the instance alone for now.
	return false, false
}

// recoveryStats reports the restart count and budget for logging.
func recoveryStats(id string) (used, budget int) {
	instanceRecoveries.Lock()
	defer instanceRecoveries.Unlock()
	st := recoveryStateLocked(id)
	return st.restarts, maxRecoveryRestarts
}

// resetRecovery clears recovery state after a successful login: the account
// starts a fresh lifecycle and deserves a full restart budget.
func resetRecovery(id string) {
	instanceRecoveries.Lock()
	delete(instanceRecoveries.m, id)
	instanceRecoveries.Unlock()
}

type WrapperInstance struct {
	Id        string    `json:"id"`
	Region    string    `json:"region"`
	Port      int       `json:"port"`
	NoRestart bool      `json:"-"`
	Cmd       *exec.Cmd `json:"-"`

	// sem bounds concurrent non-playback requests forwarded to this instance.
	// Lazily initialised by acquireInstanceSlot.
	sem chan struct{} `json:"-"`

	// playbackSem bounds /key and /m3u8, which wrapper-lite serialises
	// internally. Lazily initialised by acquirePlaybackSlot.
	playbackSem chan struct{} `json:"-"`

	// Exited is closed by the process waiter once the lite process exits.
	// KillWrapper uses it to wait for a graceful SIGTERM before escalating.
	Exited chan struct{} `json:"-"`
}

// instanceSemInitMu guards lazy semaphore creation: two concurrent first
// requests must share one channel, or the per-instance cap would silently
// double.
var instanceSemInitMu sync.Mutex

// ensureSem creates the per-instance semaphore on first use.
func (i *WrapperInstance) ensureSem() {
	if i.sem != nil {
		return
	}
	instanceSemInitMu.Lock()
	defer instanceSemInitMu.Unlock()
	if i.sem == nil {
		i.sem = make(chan struct{}, instanceConcurrency)
	}
}

// ensurePlaybackSem creates the playback semaphore on first use.
func (i *WrapperInstance) ensurePlaybackSem() {
	if i.playbackSem != nil {
		return
	}
	instanceSemInitMu.Lock()
	defer instanceSemInitMu.Unlock()
	if i.playbackSem == nil {
		i.playbackSem = make(chan struct{}, instancePlaybackConcurrency)
	}
}

// acquireInstanceSlot reserves a general concurrency slot on the instance. It
// blocks until a slot is free or the deadline passes; on timeout it returns
// false so the caller can report a busy/overloaded response instead of piling
// up.
func (i *WrapperInstance) acquireInstanceSlot(timeout time.Duration) bool {
	i.ensureSem()
	select {
	case i.sem <- struct{}{}:
		return true
	case <-time.After(timeout):
		return false
	}
}

// releaseInstanceSlot frees a previously acquired general slot.
func (i *WrapperInstance) releaseInstanceSlot() {
	if i.sem != nil {
		<-i.sem
	}
}

// acquirePlaybackSlot reserves the single playback slot for /key and /m3u8.
func (i *WrapperInstance) acquirePlaybackSlot(timeout time.Duration) bool {
	i.ensurePlaybackSem()
	select {
	case i.playbackSem <- struct{}{}:
		return true
	case <-time.After(timeout):
		return false
	}
}

// releasePlaybackSlot frees the playback slot.
func (i *WrapperInstance) releasePlaybackSlot() {
	if i.playbackSem != nil {
		<-i.playbackSem
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
	// A fresh lifecycle for this instance id: clear any FairPlay circuit left
	// over from the previous one. Restart-recovery state is deliberately kept
	// across restarts — counting signals across them is the whole point — and
	// is reset on re-login (resetRecovery) instead.
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
