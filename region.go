package main

import (
	"encoding/json"
	"fmt"
	"golang.org/x/sync/singleflight"
	"io"
	"math/rand"
	"net/http"
	"sync"
)

var (
	SongRegionCache        sync.Map
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
	if result, ok := SongRegionCache.Load(cacheKey); ok {
		return result.(bool), nil
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
		SongRegionCache.Store(cacheKey, available)
		return available, nil
	})

	if err != nil {
		return false, err
	}
	return val.(bool), nil
}

// SelectInstances returns ids of all instances whose region can serve the
// given adam ID (prefers songs, falls back to music-videos). The list is
// shuffled so concurrent requests spread across candidates instead of all
// hammering the first one.
func SelectInstances(adamId string) ([]string, error) {
	instances := SnapshotInstances()
	if len(instances) == 0 {
		return nil, nil
	}

	var selectedInstances []string
	for _, instance := range instances {
		available, err := checkAvailableOnRegion(adamId, instance.Region, false)
		if err != nil {
			return nil, err
		}
		if available {
			selectedInstances = append(selectedInstances, instance.Id)
		}
	}
	if len(selectedInstances) == 0 {
		for _, instance := range instances {
			available, err := checkAvailableOnRegion(adamId, instance.Region, true)
			if err != nil {
				return nil, err
			}
			if available {
				selectedInstances = append(selectedInstances, instance.Id)
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

// SelectInstanceForLyrics returns the id of an instance that has lyrics for
// the given adam ID and language (best effort; probe failure returns "").
func SelectInstanceForLyrics(adamId string, language string) string {
	token, err := GetToken()
	if err != nil {
		return ""
	}
	var selectedInstances []string
	for _, instance := range SnapshotInstances() {
		musicToken, err := GetMusicToken(instance)
		if err != nil {
			return ""
		}
		if HasLyrics(adamId, instance.Region, language, token, musicToken) {
			selectedInstances = append(selectedInstances, instance.Id)
		}
	}
	if len(selectedInstances) != 0 {
		return selectedInstances[rand.Intn(len(selectedInstances))]
	}
	return ""
}
