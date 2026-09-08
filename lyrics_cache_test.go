package main

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// TestHasLyricsCachedMemoizes verifies the availability cache prevents
// repeated probes for the same (song, region, language).
func TestHasLyricsCachedMemoizes(t *testing.T) {
	var probeCount atomic.Int32
	// Override hasLyricsProbe during the test window.
	orig := hasLyricsProbe
	defer func() { hasLyricsProbe = orig }()
	hasLyricsProbe = func(adamID, region, language, token, musicToken string) bool {
		probeCount.Add(1)
		return true
	}

	key := "probe-test"
	lyricsAvailabilityCache.Remove(key) // ensure clean
	_ = fmt.Sprintf                     // placeholder

	// First call probes once.
	if !hasLyricsCached("12345", "us", "en", "tk", "mt") {
		t.Fatal("expected true")
	}
	// Second call hits the cache, no new probe.
	if !hasLyricsCached("12345", "us", "en", "tk", "mt") {
		t.Fatal("expected true (cached)")
	}
	if n := probeCount.Load(); n != 1 {
		t.Errorf("expected exactly 1 probe, got %d", n)
	}
}
