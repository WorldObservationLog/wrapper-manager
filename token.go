package main

import (
	"fmt"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"os"
	"regexp"
	"time"
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
	token, ok := cache.Get("token")
	if ok {
		return token, nil
	}
	resp, err := GetHttpClient().R().Get("https://beta.music.apple.com")
	if err != nil {
		return "", err
	}

	regex := regexp.MustCompile(`/assets/index-legacy-[^/]+\.js`)
	indexJsUri := regex.FindString(resp.String())

	resp, err = GetHttpClient().R().Get("https://beta.music.apple.com" + indexJsUri)
	if err != nil {
		return "", err
	}

	regex = regexp.MustCompile(`eyJh([^"]*)`)
	token = regex.FindString(resp.String())
	cache.Add("token", token)

	return token, nil
}
