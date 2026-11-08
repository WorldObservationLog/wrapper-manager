package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

var tokenCache = expirable.NewLRU[string, string](1, nil, 24*time.Hour)

var tokenGroup singleflight.Group

var tokenFailMu sync.Mutex
var tokenFailUntil time.Time

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
// caches it for 24h. It is used only by the probe-on-miss catalog lookup.
// Singleflight prevents the first probe fan-out from scraping concurrently;
// a short failure backoff prevents a token outage from being retried by every
// missing item.
func GetToken() (string, error) {
	if token, ok := tokenCache.Get("token"); ok {
		return token, nil
	}

	tokenFailMu.Lock()
	if time.Now().Before(tokenFailUntil) {
		tokenFailMu.Unlock()
		return "", fmt.Errorf("dev token unavailable (backoff)")
	}
	tokenFailMu.Unlock()

	val, err, _ := tokenGroup.Do("token", func() (interface{}, error) {
		if token, ok := tokenCache.Get("token"); ok {
			return token, nil
		}
		token, err := scrapeToken()
		if err != nil {
			tokenFailMu.Lock()
			tokenFailUntil = time.Now().Add(60 * time.Second)
			tokenFailMu.Unlock()
			return nil, err
		}
		tokenCache.Add("token", token)
		return token, nil
	})
	if err != nil {
		return "", err
	}
	return val.(string), nil
}

func scrapeToken() (string, error) {
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
	return token, nil
}
