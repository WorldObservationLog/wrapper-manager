package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	globalHttpClient *http.Client
	httpClientOnce   sync.Once
)

func GetHttpClient() *http.Client {
	httpClientOnce.Do(func() {
		if PROXY == "" {
			globalHttpClient = http.DefaultClient
		} else {
			proxyUrl, err := url.Parse(PROXY)
			if err != nil {
				panic("Invalid proxy URL: " + PROXY)
			}
			transport := &http.Transport{
				Proxy: http.ProxyURL(proxyUrl),
				// Configure connection pool settings for high concurrency
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			}
			globalHttpClient = &http.Client{Transport: transport}
		}
	})
	return globalHttpClient
}

type LyricResponse struct {
	Errors []interface{} `json:"errors"`
	Data   []struct {
		Attributes struct {
			TtmlLocalizations string `json:"ttmlLocalizations"`
		} `json:"attributes"`
	} `json:"data"`
}

func GetLyrics(adamID string, region string, language string, token string, musicToken string) (string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s/syllable-lyrics?l[lyrics]=%s&extend=ttmlLocalizations&l[script]=en-Latn", region, adamID, language), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Music/5.7 Android/10 model/Pixel6GR1YH build/1234 (dt:66)")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("media-user-token", musicToken)
	req.Header.Set("Origin", "https://music.apple.com")
	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New(fmt.Sprintf("failed to get lyrics: %d", resp.StatusCode))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var result LyricResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("failed to get lyrics: %v", result.Errors)
	}
	if len(result.Data) == 0 {
		return "", errors.New("no data found")
	}
	ttml := result.Data[0].Attributes.TtmlLocalizations
	if ttml == "" {
		return "", errors.New("no ttml found")
	}
	return ttml, nil
}

func HasLyrics(adamID string, region string, language string, token string, musicToken string) bool {
	req, err := http.NewRequest("HEAD", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s/syllable-lyrics?l[lyrics]=%s&extend=ttmlLocalizations&l[script]=en-Latn", region, adamID, language), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Music/5.7 Android/10 model/Pixel6GR1YH build/1234 (dt:66)")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("media-user-token", musicToken)
	req.Header.Set("Origin", "https://music.apple.com")
	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	return true
}
