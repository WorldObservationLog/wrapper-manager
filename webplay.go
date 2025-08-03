package main

import (
	"errors"
)

func GetWebPlayback(adamId string, token string, musicToken string) (string, error) {
	resp, err := GetHttpClient().R().
		SetBodyJsonMarshal(map[string]string{"salableAdamId": adamId}).
		SetBearerAuthToken(token).
		SetHeader("X-Apple-Music-User-Token", musicToken).
		Post("https://play.music.apple.com/WebObjects/MZPlay.woa/wa/webPlayback")
	if err != nil {
		return "", err
	}
	var bodyJson map[string]any
	err = resp.UnmarshalJson(&bodyJson)
	if err != nil {
		return "", err
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
	resp, err := GetHttpClient().R().
		SetBodyJsonMarshal(map[string]any{"challenge": challenge, "uri": uri, "key-system": "com.widevine.alpha", "adamId": adamId, "isLibrary": false, "user-initiated": true}).
		SetBearerAuthToken(token).
		SetHeader("X-Apple-Music-User-Token", musicToken).
		Post("https://play.itunes.apple.com/WebObjects/MZPlay.woa/wa/acquireWebPlaybackLicense")
	if err != nil {
		return "", 0, err
	}
	var respJson map[string]any
	err = resp.UnmarshalJson(&respJson)
	if err != nil {
		return "", 0, err
	}
	license := respJson["license"].(string)
	renew := respJson["renew-after"].(int)
	return license, renew, nil
}
