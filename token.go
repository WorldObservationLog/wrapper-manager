package main

import (
	"fmt"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"
)

var cache = expirable.NewLRU[string, string](1, nil, time.Hour*24)

// GetMusicToken reads the MUSIC_TOKEN file of an instance (written by
// wrapper-lite into its base-dir on login / token refresh).
func GetMusicToken(instance *WrapperInstance) (string, error) {
	token, err := os.ReadFile(instanceDir(instance.Id) + "/MUSIC_TOKEN")
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// GetDevToken reads the DEV_TOKEN file of an instance, when present.
func GetDevToken(instance *WrapperInstance) (string, error) {
	token, err := os.ReadFile(instanceDir(instance.Id) + "/DEV_TOKEN")
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// GetToken scrapes a WebPlay bearer JWT from music.apple.com's index JS and
// caches it for 24h. Used for catalog region probing.
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
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex := regexp.MustCompile(`/assets/index~[^/]+\.js`)
	indexJsUri := regex.FindString(string(body))

	req, err = http.NewRequest("GET", "https://music.apple.com"+indexJsUri, nil)
	if err != nil {
		return "", err
	}

	resp, err = GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex = regexp.MustCompile(`eyJ[A-Za-z0-9-_=]+\.[A-Za-z0-9-_=]+\.[A-Za-z0-9-_=]+`)
	token := regex.FindString(string(body))
	if token == "" {
		return "", fmt.Errorf("failed to scrape dev token")
	}

	cache.Add("token", token)

	return token, nil
}
