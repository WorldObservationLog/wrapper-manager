package main

import (
	"fmt"
	"math/rand"
	"sync"
)

var (
	SongRegionCache sync.Map
	WebUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

func checkAvailableOnRegion(adamId string, region string, mv bool) bool {
	var urlType string
	if mv {
		urlType = "music-videos"
	} else {
		urlType = "songs"
	}
	cacheKey := fmt.Sprintf("%s/%s/%s", urlType, region, adamId)
	if result, ok := SongRegionCache.Load(cacheKey); ok {
		return result.(bool)
	}

	token, err := GetToken()
	if err != nil {
		return false
	}

	resp, err := GetHttpClient().R().
		SetBearerAuthToken(token).
		SetHeader("User-Agent", WebUserAgent).
		SetHeader("Origin", "https://music.apple.com").
		SetPathParam("region", region).
		SetPathParam("urltype", urlType).
		SetPathParam("adamid", adamId).
		Head("https://amp-api.music.apple.com/v1/catalog/{region}/{urltype}/{adamid}")
	if err != nil {
		return false
	}
	return resp.IsSuccessState()
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
