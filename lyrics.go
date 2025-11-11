package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

func GetHttpClient() *http.Client {
	if PROXY == "" {
		return http.DefaultClient
	}
	proxyUrl, err := url.Parse(PROXY)
	if err != nil {
		panic("Invalid proxy URL: " + PROXY)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyUrl)}
	return &http.Client{Transport: transport}
}

func GetLyrics(adamID string, region string, language string, token string, musicToken string) (string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s/lyrics?l=%s", region, adamID, language), nil)
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
	if resp.StatusCode != 200 {
		return "", errors.New(fmt.Sprintf("failed to get lyrics: %d", resp.StatusCode))
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var respJson map[string][]interface{}
	if err := json.Unmarshal(respBody, &respJson); err != nil {
		return "", err
	}
	if respJson["errors"] != nil {
		return "", errors.New(fmt.Sprintf("failed to get lyrics: %s", respJson["errors"]))
	}
	ttml := respJson["data"][0].(map[string]interface{})["attributes"].(map[string]interface{})["ttml"].(string)
	return ttml, nil
}

func HasLyrics(adamID string, region string, language string, token string, musicToken string) bool {
	req, err := http.NewRequest("HEAD", fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s/lyrics?l=%s", region, adamID, language), nil)
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
	if resp.StatusCode != 200 {
		return false
	}
	return true
}
