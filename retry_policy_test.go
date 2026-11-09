package main

import "testing"

// TestShouldRetryLiteFailure pins the retry policy: caller errors must not be
// retried, while instance-specific failures (including /lyrics 404, which can
// differ by storefront language) are worth another candidate within the
// bounded attempt budget.
func TestShouldRetryLiteFailure(t *testing.T) {
	cases := []struct {
		path string
		code int
		want bool
	}{
		// /lyrics 404 is region-specific: another storefront may have the
		// requested language, so it is retried within the bounded budget.
		{"/lyrics", 404, true},
		// Caller error -> same everywhere.
		{"/lyrics", 400, false},
		{"/m3u8", 400, false},
		{"/key", 400, false},
		// Instance-level failures -> another instance may succeed.
		{"/lyrics", 500, true},
		{"/key", 500, true},
		{"/m3u8", 404, true}, // this instance could not fetch the stream
		{"/m3u8", 500, true},
		{"/webplayback", 404, true},
		{"/webplayback", 500, true},
	}
	for _, c := range cases {
		if got := shouldRetryLiteFailure(c.path, c.code); got != c.want {
			t.Errorf("shouldRetryLiteFailure(%q, %d) = %v, want %v", c.path, c.code, got, c.want)
		}
	}
}
