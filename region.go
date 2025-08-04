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

func checkAvailableOnRegion(adamId string, region string, mv bool) bool {
	var cacheKey string
	if mv {
		cacheKey = fmt.Sprintf("mv/%s/%s", region, adamId)
	} else {
		cacheKey = fmt.Sprintf("song/%s/%s", region, adamId)
	}
	if result, ok := SongRegionCache.Load(cacheKey); ok {
		return result.(bool)
	}

	val, _, _ := songRegionSingleFlight.Do(cacheKey, func() (interface{}, error) {
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

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}
		var respJson map[string][]interface{}
		if err := json.Unmarshal(respBody, &respJson); err != nil {
			return false, err
		}

		available := respJson["data"] != nil
		SongRegionCache.Store(cacheKey, available)
		return available, nil
	})

	return val.(bool)
}

func SelectInstance(adamId string) string {
	var selectedInstances []string
	for _, instance := range Instances {
		if checkAvailableOnRegion(adamId, instance.Region, false) {
			selectedInstances = append(selectedInstances, instance.Id)
		}
	}
	if len(selectedInstances) == 0 {
		for _, instance := range Instances {
			if checkAvailableOnRegion(adamId, instance.Region, true) {
				selectedInstances = append(selectedInstances, instance.Id)
			}
		}
	}
	if len(selectedInstances) != 0 {
		return selectedInstances[rand.Intn(len(selectedInstances))]
	}
	return ""
}

func SelectInstanceForLyrics(adamId string, language string) string {
	token, err := GetToken()
	if err != nil {
		return ""
	}
	var selectedInstances []string
	for _, instance := range Instances {
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
