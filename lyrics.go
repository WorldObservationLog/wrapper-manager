package main

import (
	"errors"
	r "github.com/imroc/req/v3"
)

func GetHttpClient() *r.Client {
	client := r.C()
	if PROXY != "" {
		client.SetProxyURL(PROXY)
	}
	/*
		if DEBUG {
			client.DevMode()
		}
	*/
	return client
}

func GetLyrics(adamID string, region string, language string, token string, musicToken string) (string, error) {
	resp, err := GetHttpClient().R().
		SetPathParam("region", region).
		SetPathParam("adamid", adamID).
		SetPathParam("language", language).
		SetBearerAuthToken(token).
		SetHeader("User-Agent", "Music/5.7 Android/10 model/Pixel6GR1YH build/1234 (dt:66)").
		SetHeader("media-user-token", musicToken).
		SetHeader("Origin", "https://music.apple.com").
		Get("https://amp-api.music.apple.com/v1/catalog/{region}/songs/{adamid}/lyrics?l={language}")
	if err != nil {
		return "", err
	}
	if resp.IsErrorState() {
		return "", errors.New("no available lyrics")
	}
	var respJson map[string][]interface{}
	if err := resp.UnmarshalJson(&respJson); err != nil {
		return "", err
	}
	ttml := respJson["data"][0].(map[string]interface{})["attributes"].(map[string]interface{})["ttml"].(string)
	return ttml, nil
}
