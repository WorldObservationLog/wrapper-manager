package main

import "testing"

// TestRoutingCacheAffinityAndNegative pins the two routing signals that
// replace the old Apple availability probes: a successful instance is
// preferred for the item, a failed one is marked negative, and a later success
// clears the negative mark.
func TestRoutingCacheAffinityAndNegative(t *testing.T) {
	const adamId = "routing-cache-test-adam"
	const instanceId = "routing-cache-test-instance"

	noteInstanceSuccess(adamId, instanceId)
	if got, ok := affinityCache.Get(adamId); !ok || got != instanceId {
		t.Fatalf("affinity = %q, %v; want %q, true", got, ok, instanceId)
	}

	noteInstanceFailure(adamId, instanceId)
	if _, ok := affinityCache.Get(adamId); ok {
		t.Fatal("affinity should be cleared after the preferred instance fails")
	}
	if _, ok := negativeCache.Get(negativeKey(adamId, instanceId)); !ok {
		t.Fatal("instance should be marked negative after a failure")
	}

	noteInstanceSuccess(adamId, instanceId)
	if _, ok := negativeCache.Get(negativeKey(adamId, instanceId)); ok {
		t.Fatal("negative mark should be cleared after a success")
	}
	if got, ok := affinityCache.Get(adamId); !ok || got != instanceId {
		t.Fatalf("affinity = %q, %v; want %q, true", got, ok, instanceId)
	}
}
