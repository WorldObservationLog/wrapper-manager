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

var (
	// SongRegionCache memoizes region availability for (song|mv, region, adamId).
	// LRU-bounded (256k entries) with a 24h TTL so memory does not grow without
	// bound under a large, diverse adamId workload.
	SongRegionCache        = expirable.NewLRU[string, bool](256_000, nil, 24*time.Hour)
	songRegionSingleFlight singleflight.Group
)

// checkAvailableOnRegion probes Apple's catalog to test whether an adam ID is
// available in the given storefront region. mv switches between songs and
// music-videos. Results are memoized.
func checkAvailableOnRegion(adamId string, region string, mv bool) (bool, error) {
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
			return true, nil
		}

		var url string
		if mv {
			url = fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/music-videos/%s", region, adamId)
		} else {
			url = fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s", region, adamId)
		}
		token, err := GetToken()
		if err != nil {
			return false, err
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("User-Agent", "Mozilla/5.0 ...")
		req.Header.Set("Origin", "https://music.apple.com")

		resp, err := GetHttpClient().Do(req)
		if err != nil {
			return false, err
		}
		defer func() { _ = resp.Body.Close() }()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}
		var respJson map[string][]interface{}
		if err := json.Unmarshal(respBody, &respJson); err != nil {
			return false, err
		}

		if respJson["errors"] != nil {
			return false, nil
		}

		available := respJson["data"] != nil
		SongRegionCache.Add(cacheKey, available)
		return available, nil
	})

	if err != nil {
		return false, err
	}
	return val.(bool), nil
}

// SelectInstances returns ids of all instances whose region can serve the
// given adam ID (prefers songs, falls back to music-videos). Region probes are
// run concurrently (bounded) so first-request latency does not scale linearly
// with instance count. The list is shuffled so concurrent requests spread
// across candidates instead of all hammering the first one.
func SelectInstances(adamId string) ([]string, error) {
	instances := SnapshotInstances()
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
			ok, err := checkAvailableOnRegion(adamId, inst.Region, false)
			if err != nil {
				// On probe error treat the region as unavailable rather than
				// failing the whole request (matches old per-region semantics
				// where a single error aborted selection).
				results[i] = res{id: inst.Id, ok: false}
				return
			}
			results[i] = res{id: inst.Id, ok: ok}
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
	if len(selectedInstances) == 0 {
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

// SelectInstanceForLyrics returns the id of an instance best suited to serve
// lyrics for the given adam ID and language. Two preference tiers:
//
//  1. Instances whose storefront declares support for the requested language
//     (region -> supportedLanguageTags from the Apple storefronts API) AND
//     whose catalog has the song (region probe).
//  2. Any instance whose catalog has the song (region probe), regardless of
//     language (previous behaviour).
//
// Returns "" when nothing can serve the song.
func SelectInstanceForLyrics(adamId string, language string) string {
	instances := SnapshotInstances()
	if len(instances) == 0 {
		return ""
	}

	// Tier 1: prefer instances whose storefront supports the lyric language.
	langs := ensureStorefrontLangs()
	var tier1, tier2 []string
	for _, instance := range instances {
		if langs != nil && langs.regionSupportsLang(instance.Region, language) {
			tier1 = append(tier1, instance.Id)
		} else {
			tier2 = append(tier2, instance.Id)
		}
	}

	// Among the language-matching candidates, keep only those whose region
	// catalog actually has the song.
	if len(tier1) > 0 {
		if id := pickLyricsInstanceWithSong(adamId, language, tier1); id != "" {
			return id
		}
	}
	// Tier 2: fall back to any instance whose catalog has the song.
	if id := pickLyricsInstanceWithSong(adamId, language, tier2); id != "" {
		return id
	}
	return ""
}

// pickLyricsInstanceWithSong filters candidate instance ids to those whose
// region catalog has the song with the requested lyric language, and returns
// a random one. Returns "" if none match (or probing fails).
func pickLyricsInstanceWithSong(adamId, language string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	token, err := GetToken()
	if err != nil {
		return ""
	}
	var hit []string
	for _, id := range candidates {
		inst := GetInstance(id)
		if inst == nil {
			continue
		}
		musicToken, err := GetMusicToken(inst)
		if err != nil {
			continue
		}
		if HasLyrics(adamId, inst.Region, language, token, musicToken) {
			hit = append(hit, id)
		}
	}
	if len(hit) != 0 {
		return hit[rand.Intn(len(hit))]
	}
	return ""
}
