package main

import (
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
)

func TestParseCatalogItem(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		want      songInfo
		cacheable bool
		wantErr   bool
	}{
		{"song with lyrics", `{"data":[{"id":"1","attributes":{"hasLyrics":true,"name":"x"}}]}`, songInfo{true, true}, true, false},
		{"song without lyrics", `{"data":[{"id":"1","attributes":{"hasLyrics":false}}]}`, songInfo{true, false}, true, false},
		{"field missing", `{"data":[{"id":"1","attributes":{"name":"x"}}]}`, songInfo{true, false}, true, false},
		{"music video", `{"data":[{"id":"1","attributes":{"name":"x"}}]}`, songInfo{true, false}, true, false},
		{"not in catalog", `{"data":[]}`, songInfo{}, true, false},
		{"apple error is not cached", `{"errors":[{"status":"404"}]}`, songInfo{}, false, false},
		{"garbage", `<html>`, songInfo{}, false, true},
	}
	for _, c := range cases {
		got, cacheable, err := parseCatalogItem([]byte(c.body))
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
			continue
		}
		if got != c.want || cacheable != c.cacheable {
			t.Errorf("%s: got (%+v, cacheable=%v), want (%+v, cacheable=%v)", c.name, got, cacheable, c.want, c.cacheable)
		}
	}
}

// withInstances registers fake instances (id -> region) and a fake catalog
// lookup for the duration of a test.
func withInstances(t *testing.T, regions map[string]string, lookup func(adamId, region string, mv bool) (songInfo, error)) {
	t.Helper()
	origInst, origLookup := Instances, songLookup
	t.Cleanup(func() {
		instancesMu.Lock()
		Instances = origInst
		instancesMu.Unlock()
		songLookup = origLookup
	})
	instancesMu.Lock()
	Instances = nil
	for id, region := range regions {
		Instances = append(Instances, &WrapperInstance{Id: id, Region: region})
	}
	instancesMu.Unlock()
	songLookup = lookup
}

func sorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestSelectCandidatesLyricsFilter(t *testing.T) {
	var lookups atomic.Int32
	withInstances(t,
		map[string]string{"i-us": "us", "i-jp": "jp", "i-kr": "kr", "i-xx": "xx"},
		func(adamId, region string, mv bool) (songInfo, error) {
			lookups.Add(1)
			switch region {
			case "us":
				return songInfo{available: true, hasLyrics: true}, nil
			case "jp":
				return songInfo{available: true, hasLyrics: true}, nil
			case "kr":
				return songInfo{available: true, hasLyrics: false}, nil // song but no lyrics
			}
			return songInfo{}, nil // not in catalog
		})

	// /lyrics only goes to regions that report hasLyrics.
	got, err := selectCandidates("/lyrics", "42")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"i-jp", "i-us"}; !reflect.DeepEqual(sorted(got), want) {
		t.Errorf("/lyrics candidates = %v, want %v", sorted(got), want)
	}

	// Other endpoints still only need the song to exist.
	got, err = selectCandidates("/m3u8", "42")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"i-jp", "i-kr", "i-us"}; !reflect.DeepEqual(sorted(got), want) {
		t.Errorf("/m3u8 candidates = %v, want %v", sorted(got), want)
	}
}

// If no region advertises lyrics (or Apple stops sending the field), /lyrics
// must not fail closed: it falls back to every region that has the song.
func TestSelectCandidatesLyricsFailOpen(t *testing.T) {
	withInstances(t,
		map[string]string{"i-us": "us", "i-jp": "jp"},
		func(adamId, region string, mv bool) (songInfo, error) {
			return songInfo{available: true, hasLyrics: false}, nil
		})
	got, err := selectCandidates("/lyrics", "42")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"i-jp", "i-us"}; !reflect.DeepEqual(sorted(got), want) {
		t.Errorf("fallback candidates = %v, want %v", sorted(got), want)
	}
}
