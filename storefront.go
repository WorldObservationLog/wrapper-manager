package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// StorefrontLangs maps a storefront code (us/jp/kr/...) to the language tags
// its Apple Music store supports (e.g. jp -> ["ja","en-US"]). Used by the
// lyrics router to prefer an instance whose storefront supports the requested
// lyric language.
type StorefrontLangs map[string][]string

var (
	storefrontMu          sync.RWMutex
	storefrontData        StorefrontLangs
	storefrontFile        = "data/storefront_langs.json"
	storefrontLastAttempt time.Time
	storefrontAttemptGap  = 10 * time.Minute
)

// normalizeLangTag normalizes a language request/tag to a comparable prefix:
// "ja" -> "ja", "ja-JP" -> "ja", "zh-Hant-TW" -> "zh-Hant",
// "zh-Hans-CN" -> "zh-Hans", "en-US"/"en-GB" -> "en".
func normalizeLangTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	lower := strings.ToLower(tag)
	// Script subtags (zh-Hant*, zh-Hans*) keep the script; plain region tags
	// (en-US, ja-JP, fr-FR) collapse to the primary language.
	for _, script := range []string{"zh-hant", "zh-hans"} {
		if strings.HasPrefix(lower, script) {
			return script
		}
	}
	// Primary subtag before '-' (en-US -> en, pt-BR -> pt).
	if i := strings.IndexByte(lower, '-'); i > 0 {
		return lower[:i]
	}
	return lower
}

// regionSupportsLang reports whether a storefront region lists a language tag
// matching the requested language.
func (s StorefrontLangs) regionSupportsLang(region, language string) bool {
	tags, ok := s[strings.ToLower(region)]
	if !ok || len(tags) == 0 {
		return false
	}
	want := normalizeLangTag(language)
	if want == "" {
		return false
	}
	for _, t := range tags {
		if normalizeLangTag(t) == want {
			return true
		}
	}
	return false
}

// loadStorefrontLangsFile reads the persisted mapping from disk.
func loadStorefrontLangsFile() StorefrontLangs {
	data, err := os.ReadFile(storefrontFile)
	if err != nil {
		return nil
	}
	var m StorefrontLangs
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// saveStorefrontLangsFile persists the mapping so restarts do not need a
// fresh API call.
func saveStorefrontLangsFile(m StorefrontLangs) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll("data", 0o755); err != nil {
		return
	}
	_ = os.WriteFile(storefrontFile, data, 0o644)
}

// fetchStorefrontLangs calls the Apple Music storefronts API using a token
// borrowed from a wrapper-lite instance. wrapper-lite owns token refresh; the
// manager never scrapes music.apple.com itself.
func fetchStorefrontLangs() (StorefrontLangs, error) {
	token, err := getAnyDevToken()
	if err != nil {
		return nil, fmt.Errorf("get dev token: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.music.apple.com/v1/storefronts", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://music.apple.com")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	// This lookup is on the first /lyrics request, so keep it short: a slow
	// Apple response must not stall the request for the client's 60s timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storefronts API HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			Id         string `json:"id"`
			Attributes struct {
				SupportedLanguageTags []string `json:"supportedLanguageTags"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	m := make(StorefrontLangs, len(payload.Data))
	for _, sf := range payload.Data {
		if len(sf.Attributes.SupportedLanguageTags) > 0 {
			m[strings.ToLower(sf.Id)] = sf.Attributes.SupportedLanguageTags
		}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("storefronts API returned no data")
	}
	return m, nil
}

// ensureStorefrontLangs returns the region->language mapping, loading from
// disk first and refreshing from the API when unavailable. Failed attempts are
// retried at most every storefrontAttemptGap so a missing token cannot turn
// every lyrics request into an Apple call.
func ensureStorefrontLangs() StorefrontLangs {
	storefrontMu.RLock()
	if storefrontData != nil {
		m := storefrontData
		storefrontMu.RUnlock()
		return m
	}
	storefrontMu.RUnlock()

	if m := loadStorefrontLangsFile(); m != nil {
		storefrontMu.Lock()
		if storefrontData == nil {
			storefrontData = m
		}
		storefrontMu.Unlock()
		return m
	}

	storefrontMu.Lock()
	if !storefrontLastAttempt.IsZero() && time.Since(storefrontLastAttempt) < storefrontAttemptGap {
		storefrontMu.Unlock()
		return nil
	}
	storefrontLastAttempt = time.Now()
	storefrontMu.Unlock()

	if m, err := fetchStorefrontLangs(); err == nil {
		saveStorefrontLangsFile(m)
		storefrontMu.Lock()
		storefrontData = m
		storefrontMu.Unlock()
		return m
	} else {
		log.Warnf("failed to fetch storefront languages: %v", err)
	}
	return nil
}
