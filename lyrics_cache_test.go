package main

import (
	"sync/atomic"
	"testing"
)

// TestHasLyricsCachedMemoizes verifies the availability cache prevents
// repeated probes for the same (song, region, language, script) key.
func TestHasLyricsCachedMemoizes(t *testing.T) {
	var probeCount atomic.Int32
	// Override hasLyricsProbe during the test window.
	orig := hasLyricsProbe
	defer func() { hasLyricsProbe = orig }()
	hasLyricsProbe = func(adamID, region, language, script, token, musicToken string) bool {
		probeCount.Add(1)
		return true
	}

	// First call probes once.
	if !hasLyricsCached("12345", "us", "en", "en-Latn", "tk", "mt") {
		t.Fatal("expected true")
	}
	// Second call hits the cache, no new probe.
	if !hasLyricsCached("12345", "us", "en", "en-Latn", "tk", "mt") {
		t.Fatal("expected true (cached)")
	}
	if n := probeCount.Load(); n != 1 {
		t.Errorf("expected exactly 1 probe, got %d", n)
	}

	// An empty script normalizes to the default, so it shares the cache entry
	// created for "en-Latn" and does not probe again.
	if !hasLyricsCached("12345", "us", "en", "", "tk", "mt") {
		t.Fatal("expected true")
	}
	if n := probeCount.Load(); n != 1 {
		t.Errorf("empty script should hit the en-Latn entry, probes=%d", n)
	}

	// A different script is a different key, so it probes again.
	if !hasLyricsCached("12345", "us", "en", "ja-Latn", "tk", "mt") {
		t.Fatal("expected true")
	}
	if n := probeCount.Load(); n != 2 {
		t.Errorf("expected 2 probes after script change, got %d", n)
	}
}
