package main

import (
	"fmt"
	"net/http"
)

// HasLyrics probes whether a track has lyrics in a region for a language,
// using the same endpoint wrapper-lite /lyrics would call. Returns false on
// any probe failure (best-effort selection hint).
func HasLyrics(adamID string, region string, language string, token string, musicToken string) bool {
	url := fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/songs/%s/syllable-lyrics?l[lyrics]=%s&extend=ttmlLocalizations&l[script]=en-Latn",
		region, adamID, language)
	req, err := http.NewRequest(http.MethodHead, url, nil)
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
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}
