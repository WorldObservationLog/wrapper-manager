package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type webPlaybackResp struct {
	Errors   []any `json:"errors"`
	SongList []struct {
		HLSPlaylistURL string `json:"hls-playlist-url"`
		Assets         []struct {
			Flavor string `json:"flavor"`
			URL    string `json:"URL"`
		} `json:"assets"`
	} `json:"songList"`
}

func GetWebPlayback(adamId string, token string, musicToken string) (string, error) {
	reqBody, err := json.Marshal(map[string]string{"salableAdamId": adamId})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("POST", "https://play.music.apple.com/WebObjects/MZPlay.woa/wa/webPlayback", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("X-Apple-Music-User-Token", musicToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var bodyJson webPlaybackResp
	if err := json.Unmarshal(respBody, &bodyJson); err != nil {
		return "", err
	}
	if len(bodyJson.Errors) > 0 {
		return "", errors.New("failed to get asset")
	}
	if len(bodyJson.SongList) == 0 {
		return "", errors.New("no available asset")
	}
	song := bodyJson.SongList[0]
	if song.HLSPlaylistURL != "" {
		return song.HLSPlaylistURL, nil
	}
	for _, asset := range song.Assets {
		if asset.Flavor == "28:ctrp256" {
			return asset.URL, nil
		}
	}
	return "", errors.New("no available asset")
}

type licenseResp struct {
	Errors     []any   `json:"errors"`
	License    string  `json:"license"`
	RenewAfter float64 `json:"renew-after"`
}

func GetLicense(adamId string, challenge string, uri string, token string, musicToken string) (string, int, error) {
	reqBody, err := json.Marshal(map[string]any{"challenge": challenge, "uri": uri, "key-system": "com.widevine.alpha", "adamId": adamId, "isLibrary": false, "user-initiated": true})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequest("POST", "https://play.itunes.apple.com/WebObjects/MZPlay.woa/wa/acquireWebPlaybackLicense", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("X-Apple-Music-User-Token", musicToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}
	var respJson licenseResp
	if err := json.Unmarshal(respBody, &respJson); err != nil {
		return "", 0, err
	}
	if len(respJson.Errors) > 0 {
		return "", 0, errors.New("failed to get license")
	}
	if respJson.License == "" {
		return "", 0, errors.New("failed to get license")
	}
	return respJson.License, int(respJson.RenewAfter), nil
}
