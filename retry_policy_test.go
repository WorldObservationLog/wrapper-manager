package main

import "testing"

// TestShouldRetryLiteFailure pins the retry policy: content-level verdicts and
// caller errors must not be retried (they are identical on every instance),
// while instance-level failures are worth another candidate.
func TestShouldRetryLiteFailure(t *testing.T) {
	cases := []struct {
		path string
		code int
		want bool
	}{
		// Content-level: no lyrics in that language -> same everywhere.
		{"/lyrics", 404, false},
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
