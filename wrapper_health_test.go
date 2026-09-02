package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubscriptionDeadSignal(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"dialogHandler: {title: No Active Subscription, message: Renew...}", true},
		{"2026-09-02 19:12:21.475 [INFO ] dialogHandler: {title: No Active Subscription, ...}", true},
		{"end lease", false}, // ordinary lifecycle, MUST NOT trigger
		{"dialogHandler: {title: Check the account information you entered and try again., message: }", false},
		{"credentialHandler: {title: , message: , 2FA: false}", false},
		{"request: GET /m3u8?adamId=1490256995", false},
	}
	for _, c := range cases {
		if got := isSubscriptionDeadSignal(c.line); got != c.want {
			t.Errorf("isSubscriptionDeadSignal(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestSessionInvalidSignal(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"dialogHandler: {title: Check the account information you entered and try again., message: }", true},
		{"dialogHandler: {title: Sign In, message: }", false}, // plain sign-in prompt alone is not fatal
		{"No Active Subscription", false},                     // belongs to the other tier
		{"end lease", false},
		{"wrapper-lite listening on 127.0.0.1:51023", false},
	}
	for _, c := range cases {
		if got := isSessionInvalidSignal(c.line); got != c.want {
			t.Errorf("isSessionInvalidSignal(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// TestSessionInvalidKeepsData ensures deactivation removes the instance from
// the registry but leaves the on-disk account data for a later re-login.
func TestSessionInvalidKeepsData(t *testing.T) {
	id := "test-session-invalid"
	dir := instanceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(dir)) // clean parent chain best-effort

	marker := filepath.Join(dir, "STOREFRONT_ID")
	if err := os.WriteFile(marker, []byte("143441"), 0o644); err != nil {
		t.Fatal(err)
	}

	inst := &WrapperInstance{Id: id, Region: "us"}
	InsertInstance(inst)
	defer RemoveInstance(inst)

	handleSessionInvalid(inst, "test")

	if GetInstance(id) != nil {
		t.Errorf("instance %s should have been removed from the registry", id)
	}
	if !inst.NoRestart {
		t.Errorf("instance should be marked NoRestart")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("data directory should be KEPT for session-invalid accounts: %v", err)
	}
}

var _ = strings.TrimSpace // keep strings import
