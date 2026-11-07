package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Per-endpoint admission control and timeouts.
//
// /key and /m3u8 are serialized inside wrapper-lite by a single playback mutex
// (g_playback_mutex), so the manager admits exactly one request per instance
// for those paths. Other endpoints use the broader per-instance cap.
const (
	playbackSlotTimeout     = 2 * time.Second
	otherSlotTimeout        = 5 * time.Second
	playbackUpstreamTimeout = 10 * time.Second
	otherUpstreamTimeout    = 12 * time.Second
)

func isPlaybackPath(path string) bool {
	return path == "/key" || path == "/m3u8"
}

// fetchFromLite performs a request against one lite instance and returns the
// raw response body plus the lite business code. Nothing is written to w, so
// callers may retry on another instance before emitting a response.
//
// Admission is per instance: one playback slot for /key and /m3u8 (matching
// lite's global playback mutex), and the general instanceConcurrency slots for
// the other endpoints.
func fetchFromLite(inst *WrapperInstance, method, path string, query url.Values, body []byte) (respBody []byte, liteCode int, err error) {
	playback := isPlaybackPath(path)
	slotTimeout := otherSlotTimeout
	upstreamTimeout := otherUpstreamTimeout
	acquire := inst.acquireInstanceSlot
	release := inst.releaseInstanceSlot
	if playback {
		slotTimeout = playbackSlotTimeout
		upstreamTimeout = playbackUpstreamTimeout
		acquire = inst.acquirePlaybackSlot
		release = inst.releasePlaybackSlot
	}
	if !acquire(slotTimeout) {
		return nil, -1, fmt.Errorf("instance %s busy", shortID(inst.Id))
	}
	defer release()

	target := fmt.Sprintf("http://127.0.0.1:%d%s", inst.Port, path)
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	req.Header.Set("Content-Type", "application/json")

	// Bound a single forwarded request so a hung lite call cannot hold the
	// per-instance slot (and thus the whole manager) indefinitely. Healthy
	// requests complete within ~1s; playback gets a tighter bound because the
	// lite side serializes it.
	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return nil, -1, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, -1, err
	}
	var envelope struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(respBody, &envelope)
	return respBody, envelope.Code, nil
}

// prefetchKeyURI is the shared context-template key URI used by upstream
// wrapper-lite (adamId=0). Requesting it with a real adamId is rejected by
// upstream because the resulting key cannot decrypt any track.
const prefetchKeyURI = "skd://itunes.apple.com/P000000000/s1/e1"

// validateKeyRequest enforces the upstream wrapper-lite /key contract:
// adamId is required, uri is required, and the prefetch context-template URI
// may only be used with adamId=0. Validating here fails bad requests fast,
// without selecting and calling a lite instance.
func validateKeyRequest(q url.Values) error {
	adamId := q.Get("adamId")
	if adamId == "" {
		return fmt.Errorf("missing adamId")
	}
	uri := q.Get("uri")
	if uri == "" {
		return fmt.Errorf("missing uri")
	}
	if adamId != "0" && uri == prefetchKeyURI {
		return fmt.Errorf("invalid uri for adamId")
	}
	return nil
}

// cacheablePaths are the wrapper-lite resource endpoints Cloudflare is
// allowed to cache on success. /license, /login, /logout and /status are
// deliberately excluded.
func isCacheablePath(path string) bool {
	switch path {
	case "/m3u8", "/webplayback", "/key", "/lyrics":
		return true
	}
	return false
}

// writeLiteBody writes a raw lite response body through to the client with an
// appropriate Cache-Control header (public on cacheable success, no-store on
// any failure / non-cacheable endpoint).
func writeLiteBody(w http.ResponseWriter, path string, respBody []byte, liteCode int) {
	setCacheHeader(w, isCacheablePath(path), liteCode)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}

// Retry policy for lite business failures. A request is retried on another
// instance only when the failure could plausibly differ per instance.
//
// /lyrics 404 is region-specific in both directions: one storefront can have
// the requested language while another does not. It is therefore retryable
// here, but the attempt count and the overall budget keep a genuinely
// lyric-less song from walking the whole pool.
const (
	maxRetryAttempts   = 3
	overallRetryBudget = 18 * time.Second
)

// shouldRetryLiteFailure reports whether a lite business failure (non-zero
// code) is worth retrying on another instance.
//
// Not retryable: 400 - the caller's request is malformed; every instance
// returns the same.
func shouldRetryLiteFailure(path string, code int) bool {
	if code == 400 {
		return false
	}
	return true
}

// selectAndProxy picks an instance able to serve adamId and proxies the
// request to it. Candidate order comes from the routing cache: last successful
// instance first, then instances not known to have failed for this item, then
// the rest. Up to maxRetryAttempts candidates are tried inside the overall
// budget.
func selectAndProxy(w http.ResponseWriter, r *http.Request, path string, adamID string, body []byte) {
	selectAndProxyCandidates(w, r, path, adamID, adamID, candidateIDs(adamID), body)
}

// selectAndProxyCandidates is selectAndProxy with an explicit candidate list
// and routing-cache key (used by /lyrics for language-preferred ordering).
func selectAndProxyCandidates(w http.ResponseWriter, r *http.Request, path string, adamID string, routeKey string, candidates []string, body []byte) {
	if len(candidates) == 0 {
		WriteLiteError(w, "no available instance")
		return
	}

	query := r.URL.Query()
	var lastBody []byte
	var lastCode int
	attempts := 0
	deadline := time.Now().Add(overallRetryBudget)
	for i, id := range candidates {
		if attempts >= maxRetryAttempts || time.Now().After(deadline) {
			break
		}
		inst := GetInstance(id)
		if inst == nil {
			continue
		}
		attempts++
		respBody, code, ferr := fetchFromLite(inst, r.Method, path, query, body)
		if ferr != nil {
			log.Warnf("%s on instance %s transport error: %v", path, shortID(id), ferr)
			noteInstanceFailure(routeKey, id)
			continue
		}
		if code == 0 {
			noteInstanceSuccess(routeKey, id)
			writeLiteBody(w, path, respBody, code)
			return
		}
		retryable := shouldRetryLiteFailure(path, code)
		if retryable {
			noteInstanceFailure(routeKey, id)
		}
		// Return on the final candidate or when the failure is a caller error
		// that another instance cannot change.
		if i == len(candidates)-1 || !retryable {
			writeLiteBody(w, path, respBody, code)
			return
		}
		lastBody = respBody
		lastCode = code
		log.Infof("%s on instance %s failed (code %d); trying another", path, shortID(id), code)
	}
	if lastBody != nil {
		writeLiteBody(w, path, lastBody, lastCode)
		return
	}
	WriteLiteError(w, "no available instance")
}

// handleLiteEndpoint is the generic resource-endpoint handler.
func handleLiteEndpoint(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	log.Infof("%s %s from %s", r.Method, path, r.RemoteAddr)

	switch path {
	case "/status":
		handleStatus(w, r)
		return
	case "/m3u8", "/webplayback":
		adamId := r.URL.Query().Get("adamId")
		if adamId == "" {
			WriteLiteError(w, "missing adamId")
			return
		}
		selectAndProxy(w, r, path, adamId, nil)
	case "/key":
		if verr := validateKeyRequest(r.URL.Query()); verr != nil {
			WriteLiteError(w, verr.Error())
			return
		}
		selectAndProxy(w, r, path, r.URL.Query().Get("adamId"), nil)
	case "/lyrics":
		adamId := r.URL.Query().Get("adamId")
		if adamId == "" {
			WriteLiteError(w, "missing adamId")
			return
		}
		language := r.URL.Query().Get("language")
		if language == "" {
			language = "en"
		}
		script := r.URL.Query().Get("script")
		// Language-preferred candidates first; the lite business response is
		// the availability signal, so no separate HEAD probe is needed.
		selectAndProxyCandidates(w, r, path, adamId, lyricsRouteKey(adamId, language, script), lyricsCandidateIDs(adamId, language, script), nil)
	case "/license":
		if r.Method != http.MethodPost {
			WriteLiteError(w, "method not allowed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			WriteLiteError(w, "invalid body")
			return
		}
		var payload struct {
			AdamId    string `json:"adamId"`
			Challenge string `json:"challenge"`
			Uri       string `json:"uri"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			WriteLiteError(w, "invalid JSON body")
			return
		}
		if payload.AdamId == "" || payload.Challenge == "" || payload.Uri == "" {
			WriteLiteError(w, "missing adamId, challenge, or uri")
			return
		}
		selectAndProxy(w, r, path, payload.AdamId, body)
	default:
		WriteLiteError(w, "not found")
	}
}

// handleStatus aggregates the regions of all running instances. The data
// shape keeps the field names of the previous manager's StatusData so older
// tooling that parses them keeps working.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	instances := SnapshotInstances()
	regionSeen := make(map[string]bool)
	regions := make([]string, 0)
	for _, inst := range instances {
		if inst.Region == "" {
			continue
		}
		if !regionSeen[strings.ToUpper(inst.Region)] {
			regionSeen[strings.ToUpper(inst.Region)] = true
			regions = append(regions, inst.Region)
		}
	}
	WriteLiteSuccess(w, map[string]any{
		"status":      len(instances) != 0,
		"regions":     regions,
		"clientCount": len(instances),
		"ready":       isReady(),
		// Instances temporarily excluded after repeated FairPlay failures.
		"degraded": degradedCount(),
	})
}
