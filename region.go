package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

var (
	// SongRegionCache 带 TTL + 容量上限，避免 adamId×region 组合无限驻留导致内存泄漏。
	// 区域可用性基本静态，24h TTL 足够；上限 10 万条防止突发流量打爆内存。
	SongRegionCache        = expirable.NewLRU[string, bool](100000, nil, 24*time.Hour)
	songRegionSingleFlight singleflight.Group
)

// regionCanServe 判断某 region 是否能提供该 adamId：先查歌曲，歌曲不可用时回退查
// music-video（等价于旧版 SelectInstance 的 song→mv 两轮逻辑）。song 查询出错直接
// 返回错误，与旧逻辑一致；song 不可用且无错误时才继续查 mv。
func regionCanServe(adamId string, region string) (bool, error) {
	ok, err := checkAvailableOnRegion(adamId, region, false)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	return checkAvailableOnRegion(adamId, region, true)
}

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

		var apiUrl string
		if mv {
			apiUrl = fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/music-videos/%s", region, adamId)
		} else {
			apiUrl = fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s", region, adamId)
		}
		token, err := GetToken()
		if err != nil {
			return false, err
		}

		// 独立超时：region 探测是辅助逻辑，不应阻塞主请求太久。
		// Apple API 正常 1-2s 响应，10s 足以覆盖慢网络；超过则视为不可用。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", apiUrl, nil)
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
		defer resp.Body.Close()

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
		if available {
			SongRegionCache.Add(cacheKey, true)
		}
		return available, nil
	})

	return val.(bool), err
}
