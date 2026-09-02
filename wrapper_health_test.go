package main

import (
	"strings"
	"testing"
)

func TestIsAccountFailureSignal(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"dialogHandler: {title: No Active Subscription, message: Renew...}", true},
		{"2026-09-02 19:12:21.475 [INFO ] dialogHandler: {title: No Active Subscription, ...}", true},
		{"end lease", false}, // MUST NOT trigger (ordinary lifecycle)
		{"dialogHandler: {title: Check the account information you entered and try again., message: }", false}, // v1 did not remove on this
		{"credentialHandler: {title: , message: , 2FA: false}", false},
		{"wrapper-lite listening on 127.0.0.1:51023", false},
		{"request: GET /m3u8?adamId=1490256995", false},
	}
	for _, c := range cases {
		if got := isAccountFailureSignal(c.line); got != c.want {
			t.Errorf("isAccountFailureSignal(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestHandleUnhealthyRemovesInstance(t *testing.T) {
	id := "test-unhealthy-id"
	inst := &WrapperInstance{Id: id, Region: "us"}
	InsertInstance(inst)
	defer RemoveInstance(inst)

	handleUnhealthyInstance(inst, "test")

	if GetInstance(id) != nil {
		t.Errorf("instance %s should have been removed", id)
	}
	if !inst.NoRestart {
		t.Errorf("instance should be marked NoRestart")
	}
	_ = strings.TrimSpace // keep strings import if unused elsewhere
}
