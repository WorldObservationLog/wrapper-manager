package main

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestFairplayCircuitBreaker verifies the circuit stays closed below the
// threshold, opens at it, and does not re-trigger while already open.
func TestFairplayCircuitBreaker(t *testing.T) {
	id := "circuit-test"
	resetFairplayCircuit(id)

	if circuitOpen(id) {
		t.Fatal("circuit should start closed")
	}

	for i := 1; i < fairplayFailThreshold; i++ {
		if recordFairplayFailure(id) {
			t.Fatalf("circuit opened early at failure %d", i)
		}
		if circuitOpen(id) {
			t.Fatalf("circuit should still be closed after %d failures", i)
		}
	}

	if !recordFairplayFailure(id) {
		t.Fatal("circuit should open at the threshold")
	}
	if !circuitOpen(id) {
		t.Fatal("circuit should report open after threshold")
	}

	// Further failures while open must not re-trigger the "just opened" signal.
	if recordFairplayFailure(id) {
		t.Error("circuit should not re-trigger while already open")
	}
}

// TestFairplayCircuitAutoRecovers verifies an expired circuit closes itself and
// the failure count restarts from zero.
func TestFairplayCircuitAutoRecovers(t *testing.T) {
	id := "circuit-recover"
	resetFairplayCircuit(id)

	// Force an already-expired open state.
	fairplayCircuits.Lock()
	fairplayCircuits.m[id] = &fairplayCircuitState{openUntil: time.Now().Add(-time.Second)}
	fairplayCircuits.Unlock()

	if circuitOpen(id) {
		t.Fatal("expired circuit should report closed")
	}
	// After recovery the counter restarts, so one failure must not open it.
	if recordFairplayFailure(id) {
		t.Error("first failure after recovery should not open the circuit")
	}
}

// TestResetFairplayCircuit verifies a fresh instance lifecycle clears state.
func TestResetFairplayCircuit(t *testing.T) {
	id := "circuit-reset"
	for i := 0; i < fairplayFailThreshold; i++ {
		recordFairplayFailure(id)
	}
	if !circuitOpen(id) {
		t.Fatal("circuit should be open")
	}
	resetFairplayCircuit(id)
	if circuitOpen(id) {
		t.Error("reset should clear the circuit")
	}
}

// TestIsWrapperRelayNoise verifies high-volume lite relay lines are dropped
// while actionable lines (manager WARN+, lite ERROR/WARN relay) are kept.
func TestIsWrapperRelayNoise(t *testing.T) {
	cases := []struct {
		level logrus.Level
		msg   string
		want  bool
	}{
		// Dropped: lite INFO relay noise (logged by the manager at INFO).
		{logrus.InfoLevel, "[wrapper abc123] 2026-09-26 12:51:10.064 [INFO ] request: GET /key?adamId=1", true},
		{logrus.InfoLevel, "[wrapper abc123] 2026-09-26 12:51:10.354 [INFO ] adamId: 1422652341, uri: skd://x", true},
		{logrus.InfoLevel, "[wrapper abc123] 2026-09-26 12:51:10.062 [DEBUG] something", true},
		// Kept: lite errors/warnings relayed at INFO but carrying lite severity.
		{logrus.InfoLevel, "[wrapper abc123] 2026-09-26 12:51:10.062 [ERROR] handler exception: Fairplay error. KDCanProcessCKC status: -42786", false},
		{logrus.InfoLevel, "[wrapper abc123] 2026-09-26 12:51:10.062 [WARN ] 2FA code timeout", false},
		// Kept: manager's own log lines at INFO.
		{logrus.InfoLevel, "/key on instance abc123 failed (code 500); trying another", false},
		{logrus.InfoLevel, "wrapperManager running at 0.0.0.0:32767", false},
		// Kept: manager health events logged at WARN, even with the wrapper prefix.
		{logrus.WarnLevel, "[wrapper abc123] fairplay failures reached 5; excluding instance for 5m0s (account kept)", false},
		{logrus.WarnLevel, "[wrapper abc123] session invalid (...); deactivating instance (data kept)", false},
		{logrus.ErrorLevel, "[wrapper abc123] failed to start after login: x", false},
	}
	for _, c := range cases {
		entry := &logrus.Entry{Level: c.level, Message: c.msg}
		if got := isWrapperRelayNoise(entry); got != c.want {
			t.Errorf("isWrapperRelayNoise(%v, %q) = %v, want %v", c.level, c.msg, got, c.want)
		}
	}
}

// TestFairplayFailureSignalDetection covers the log patterns lite emits when
// Apple refuses the content key.
func TestFairplayFailureSignalDetection(t *testing.T) {
	yes := []string{
		"2026-09-26 12:51:10.062 [ERROR] handler exception: Fairplay error. KDCanProcessCKC status: -42786",
		"Fairplay error",
		"KDCanProcessCKC status: -42786",
	}
	for _, s := range yes {
		if !isFairplayFailureSignal(s) {
			t.Errorf("expected fairplay signal: %q", s)
		}
	}
	no := []string{
		"[INFO ] adamId: 1422652341, uri: skd://x",
		"No Active Subscription",
		"end lease",
	}
	for _, s := range no {
		if isFairplayFailureSignal(s) {
			t.Errorf("unexpected fairplay signal: %q", s)
		}
	}
}
