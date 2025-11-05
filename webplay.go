package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

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
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var bodyJson map[string]any
	err = json.Unmarshal(respBody, &bodyJson)
	if err != nil {
		return "", err
	}
	if bodyJson["errors"] != nil {
		return "", errors.New("failed to get asset")
	}
	if playlist, ok := bodyJson["songList"].([]any)[0].(map[string]interface{})["hls-playlist-url"]; ok {
		return playlist.(string), nil
	}
	assets := bodyJson["songList"].([]any)[0].(map[string]interface{})["assets"].([]any)
	for _, asset := range assets {
		if asset.(map[string]interface{})["flavor"].(string) == "28:ctrp256" {
			return asset.(map[string]interface{})["URL"].(string), nil
		}
	}
	return "", errors.New("no available asset")
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
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}
	var respJson map[string]any
	err = json.Unmarshal(respBody, &respJson)
	if err != nil {
		return "", 0, err
	}
	if respJson["errors"] != nil {
		return "", 0, errors.New("failed to get license")
	}
	license := respJson["license"].(string)
	renew := int(respJson["renew-after"].(float64))
	return license, renew, nil
}
