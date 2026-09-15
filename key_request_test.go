package main

import (
	"net/url"
	"testing"
)

// TestValidateKeyRequest covers the upstream /key contract as adapted by the
// manager: adamId required, uri required, and the prefetch URI only valid
// with adamId=0.
func TestValidateKeyRequest(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{"valid real key", "adamId=1490256995&uri=skd%3A%2F%2Fitunes.apple.com%2FP302292056%2Fc6", ""},
		{"missing adamId", "uri=skd%3A%2F%2Fx", "missing adamId"},
		{"missing uri", "adamId=1490256995", "missing uri"},
		{"both missing", "", "missing adamId"},
		{"prefetch with real adamId rejected", "adamId=1490256995&uri=skd%3A%2F%2Fitunes.apple.com%2FP000000000%2Fs1%2Fe1", "invalid uri for adamId"},
		{"prefetch with adamId=0 allowed", "adamId=0&uri=skd%3A%2F%2Fitunes.apple.com%2FP000000000%2Fs1%2Fe1", ""},
		{"other uri with adamId=0 allowed", "adamId=0&uri=skd%3A%2F%2Fother", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := url.ParseQuery(c.query)
			if err != nil {
				t.Fatalf("bad test query: %v", err)
			}
			got := validateKeyRequest(q)
			if c.wantErr == "" {
				if got != nil {
					t.Errorf("expected no error, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected error %q, got nil", c.wantErr)
			}
			if got.Error() != c.wantErr {
				t.Errorf("got error %q, want %q", got.Error(), c.wantErr)
			}
		})
	}
}
