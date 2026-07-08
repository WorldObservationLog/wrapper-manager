package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

var cache = expirable.NewLRU[string, string](1, nil, time.Hour*24)

func GetMusicToken(instance *WrapperInstance) (string, error) {
	token, err := os.ReadFile(fmt.Sprintf("data/wrapper/rootfs/data/instances/%s/MUSIC_TOKEN", instance.Id))
	if err != nil {
		return "", err
	}
	return string(token), nil
}

func GetToken() (string, error) {
	if token, ok := cache.Get("token"); ok {
		return token, nil
	}
	req, err := http.NewRequest("GET", "https://music.apple.com", nil)
	if err != nil {
		return "", err
	}

	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("failed to close response body: %v\n", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex := regexp.MustCompile(`/assets/index~[^/]+\.js`)
	indexJsUri := regex.FindString(string(body))
	if indexJsUri == "" {
		// 未能从首页定位到 index JS：Apple 改版或响应异常。不缓存，下次重试。
		return "", fmt.Errorf("failed to locate index js in music.apple.com homepage")
	}

	req, err = http.NewRequest("GET", "https://music.apple.com"+indexJsUri, nil)
	if err != nil {
		return "", err
	}

	resp, err = GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("failed to close response body: %v\n", err)
		}
	}()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex = regexp.MustCompile(`eyJ[A-Za-z0-9-_=]+\.[A-Za-z0-9-_=]+\.[A-Za-z0-9-_=]+`)
	token := regex.FindString(string(body))
	if token == "" {
		// 关键修复：抓取失败时绝不缓存空 token。
		// 否则空串会被缓存 24h，导致此后所有 amp-api 请求持续 401，
		// 表现为长时间运行后 M3U8/Lyrics/WebPlayback 全线 "no available instance"。
		return "", fmt.Errorf("failed to extract bearer token from index js (apple may have changed page structure)")
	}

	cache.Add("token", token)

	return token, nil
}
