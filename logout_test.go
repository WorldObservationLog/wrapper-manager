package main

import (
	"github.com/gofrs/uuid/v5"
	"testing"
)

// TestResolveLogoutID covers username, UUID and invalid inputs.
func TestResolveLogoutID(t *testing.T) {
	username := "acct@example.com"
	fromUsername := InstanceID(username)

	// Username resolves to the UUIDv5 of the account.
	got, err := resolveLogoutID(LogoutRequest{Username: username})
	if err != nil {
		t.Fatalf("username: unexpected error: %v", err)
	}
	if got != fromUsername {
		t.Errorf("username: got %s, want %s", got, fromUsername)
	}

	// A raw UUID is used as-is (normalized).
	raw := uuid.Must(uuid.NewV4()).String()
	got, err = resolveLogoutID(LogoutRequest{Id: raw})
	if err != nil {
		t.Fatalf("id: unexpected error: %v", err)
	}
	if got != raw {
		t.Errorf("id: got %s, want %s", got, raw)
	}

	// id wins when both are supplied.
	got, err = resolveLogoutID(LogoutRequest{Username: username, Id: raw})
	if err != nil {
		t.Fatalf("both: unexpected error: %v", err)
	}
	if got != raw {
		t.Errorf("both: got %s, want id %s", got, raw)
	}

	// Invalid UUID is rejected.
	if _, err := resolveLogoutID(LogoutRequest{Id: "not-a-uuid"}); err == nil {
		t.Error("invalid id should return an error")
	}

	// Neither field is rejected.
	if _, err := resolveLogoutID(LogoutRequest{}); err == nil {
		t.Error("empty request should return an error")
	}
}
