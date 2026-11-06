package main

import (
	"encoding/json"
	"fmt"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// songInfo is what one catalog lookup tells us about an adam ID in one
// storefront region.
type songInfo struct {
	// available: the catalog has the item in this region.
	available bool
	// hasLyrics: data[0].attributes.hasLyrics from the same response (songs
	// only; always false for music-videos).
	hasLyrics bool
}

var (
	// SongRegionCache memoizes catalog lookups for (song|mv, region, adamId).
	// LRU-bounded (256k entries) with a 24h TTL so memory does not grow without
	// bound under a large, diverse adamId workload.
	SongRegionCache        = expirable.NewLRU[string, songInfo](256_000, nil, 24*time.Hour)
	songRegionSingleFlight singleflight.Group
)

// parseCatalogItem interprets a catalog songs/music-videos response. cacheable
// is false for an Apple "errors" payload, which may be transient and must not
// be remembered as "not available".
func parseCatalogItem(body []byte) (info songInfo, cacheable bool, err error) {
	var payload struct {
		Errors []json.RawMessage `json:"errors"`
		Data   []struct {
			Attributes struct {
				HasLyrics bool `json:"hasLyrics"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return songInfo{}, false, err
	}
	if payload.Errors != nil {
		return songInfo{}, false, nil
	}
	if len(payload.Data) == 0 {
		return songInfo{}, true, nil
	}
	return songInfo{available: true, hasLyrics: payload.Data[0].Attributes.HasLyrics}, true, nil
}

// lookupSong queries Apple's catalog for an adam ID in the given storefront
// region. mv switches between songs and music-videos. Results are memoized.
func lookupSong(adamId string, region string, mv bool) (songInfo, error) {
	var cacheKey string
	if mv {
		cacheKey = fmt.Sprintf("mv/%s/%s", region, adamId)
	} else {
		cacheKey = fmt.Sprintf("song/%s/%s", region, adamId)
	}
	if result, ok := SongRegionCache.Get(cacheKey); ok {
		return result, nil
	}

	val, err, _ := songRegionSingleFlight.Do(cacheKey, func() (interface{}, error) {
		if adamId == "0" {
			return songInfo{available: true}, nil
		}

		var url string
		if mv {
			url = fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/music-videos/%s", region, adamId)
		} else {
			url = fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s", region, adamId)
		}
		token, err := GetToken()
		if err != nil {
			return songInfo{}, err
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return songInfo{}, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("User-Agent", "Mozilla/5.0 ...")
		req.Header.Set("Origin", "https://music.apple.com")

		resp, err := GetHttpClient().Do(req)
		if err != nil {
			return songInfo{}, err
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return songInfo{}, err
		}
		info, cacheable, err := parseCatalogItem(respBody)
		if err != nil {
			return songInfo{}, err
		}
		if cacheable {
			SongRegionCache.Add(cacheKey, info)
		}
		return info, nil
	})

	if err != nil {
		return songInfo{}, err
	}
	return val.(songInfo), nil
}

// songLookup is the catalog lookup used by instance selection; tests may swap
// it.
var songLookup = lookupSong

// checkAvailableOnRegion reports whether an adam ID is available in the given
// storefront region.
func checkAvailableOnRegion(adamId string, region string, mv bool) (bool, error) {
	info, err := songLookup(adamId, region, mv)
	return info.available, err
}

// selectableInstances returns the registered instances that are currently
// eligible to serve requests: instances whose FairPlay circuit is open (after
// repeated key-processing failures) are skipped until the cooldown elapses.
func selectableInstances() []*WrapperInstance {
	all := SnapshotInstances()
	out := make([]*WrapperInstance, 0, len(all))
	for _, inst := range all {
		if circuitOpen(inst.Id) {
			continue
		}
		out = append(out, inst)
	}
	return out
}

// SelectInstances returns ids of all instances whose region can serve the
// given adam ID (prefers songs, falls back to music-videos). Region probes are
// run concurrently (bounded) so first-request latency does not scale linearly
// with instance count. The list is shuffled so concurrent requests spread
// across candidates instead of all hammering the first one.
func SelectInstances(adamId string) ([]string, error) {
	return selectInstances(adamId, false)
}

// SelectInstancesWithLyrics is SelectInstances restricted to instances whose
// region reports hasLyrics for the song. It reuses the same catalog lookup (and
// cache) as SelectInstances, so it costs no extra Apple requests. Music-videos
// carry no lyrics, so there is no music-video fallback.
func SelectInstancesWithLyrics(adamId string) ([]string, error) {
	return selectInstances(adamId, true)
}

func selectInstances(adamId string, needLyrics bool) ([]string, error) {
	instances := selectableInstances()
	if len(instances) == 0 {
		return nil, nil
	}

	// Probe all instances' regions for the song in parallel, bounded.
	const probeWorkers = 8
	sem := make(chan struct{}, probeWorkers)
	type res struct {
		id string
		ok bool
	}
	results := make([]res, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func(i int, inst *WrapperInstance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := songLookup(adamId, inst.Region, false)
			if err != nil {
				// On probe error treat the region as unavailable rather than
				// failing the whole request (matches old per-region semantics
				// where a single error aborted selection).
				results[i] = res{id: inst.Id, ok: false}
				return
			}
			results[i] = res{id: inst.Id, ok: info.available && (!needLyrics || info.hasLyrics)}
		}(i, inst)
	}
	wg.Wait()

	var selectedInstances []string
	for _, r := range results {
		if r.ok {
			selectedInstances = append(selectedInstances, r.id)
		}
	}
	// Fall back to music-videos only when no song hit was found.
	if len(selectedInstances) == 0 && !needLyrics {
		for _, inst := range instances {
			ok, err := checkAvailableOnRegion(adamId, inst.Region, true)
			if err != nil {
				return nil, err
			}
			if ok {
				selectedInstances = append(selectedInstances, inst.Id)
			}
		}
	}
	// Shuffle so parallel requests do not all select the same instance.
	rand.Shuffle(len(selectedInstances), func(i, j int) {
		selectedInstances[i], selectedInstances[j] = selectedInstances[j], selectedInstances[i]
	})
	return selectedInstances, nil
}

// SelectInstance returns a single id of an instance whose region can serve
// the given adam ID, or "" when none is available.
func SelectInstance(adamId string) (string, error) {
	ids, err := SelectInstances(adamId)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}
