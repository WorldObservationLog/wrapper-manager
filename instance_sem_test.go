package main

import (
	"sync"
	"testing"
	"time"
)

// TestInstanceSemaphoreCapsConcurrency verifies the per-instance semaphore
// admits at most instanceConcurrency concurrent holders.
func TestInstanceSemaphoreCapsConcurrency(t *testing.T) {
	inst := &WrapperInstance{Id: "sem-test"}
	inst.ensureSem()

	var mu sync.Mutex
	active := 0
	maxActive := 0
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !inst.acquireInstanceSlot(5 * time.Second) {
				t.Error("failed to acquire slot")
				return
			}
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond) // hold the slot briefly

			mu.Lock()
			active--
			mu.Unlock()
			inst.releaseInstanceSlot()
		}()
	}
	wg.Wait()

	if maxActive > instanceConcurrency {
		t.Errorf("max concurrent = %d, want <= %d", maxActive, instanceConcurrency)
	}
}

// TestInstanceSemaphoreTimeout verifies that when all slots are busy a timed
// acquisition fails instead of blocking forever.
func TestInstanceSemaphoreTimeout(t *testing.T) {
	inst := &WrapperInstance{Id: "sem-timeout"}
	inst.ensureSem()
	// Fill all slots.
	for i := 0; i < instanceConcurrency; i++ {
		if !inst.acquireInstanceSlot(time.Second) {
			t.Fatal("failed to fill slots")
		}
	}
	// Next acquisition must time out quickly.
	start := time.Now()
	if inst.acquireInstanceSlot(100 * time.Millisecond) {
		t.Error("acquisition should have timed out when all slots busy")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
	// Release one slot; acquisition should now succeed.
	inst.releaseInstanceSlot()
	if !inst.acquireInstanceSlot(time.Second) {
		t.Error("acquisition should succeed after a slot frees")
	}
}
