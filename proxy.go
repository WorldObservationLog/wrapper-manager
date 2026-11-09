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

// isProbeOnMissPath reports whether a 404 from lite should trigger one
// catalog lookup before continuing. Catalog availability and hasLyrics are
// cheap relative to a blind walk over the instance pool.
func isProbeOnMissPath(path string) bool {
	return path == "/m3u8" || path == "/lyrics"
}

// probeCandidates is the probe-on-miss catalog lookup. Tests may swap it.
var probeCandidates = func(path, adamID string) ([]string, error) {
	switch path {
	case "/lyrics":
		return SelectInstancesWithLyrics(adamID)
	case "/m3u8":
		return SelectInstances(adamID)
	default:
		return nil, nil
	}
}

// routeKeyForRequest keeps lyrics routing separate per language/script so a
// 404 for one language does not poison affinity for another.
func routeKeyForRequest(path string, r *http.Request, adamID string) string {
	if path != "/lyrics" {
		return adamID
	}
	language := r.URL.Query().Get("language")
	if language == "" {
		language = "en"
	}
	return lyricsRouteKey(adamID, language, r.URL.Query().Get("script"))
}

// selectAndProxy picks an instance able to serve adamId and proxies the
// request to it. Candidate order comes from the routing cache: last successful
// instance first, then instances not known to have failed for this item, then
// the rest. For /m3u8 and /lyrics, a 404 triggers one catalog lookup so the
// retry goes to a region that actually has the item (or lyrics) instead of
// blindly spending the remaining attempts.
func selectAndProxy(w http.ResponseWriter, r *http.Request, path string, adamID string, body []byte) {
	selectAndProxyCandidates(w, r, path, adamID, routeKeyForRequest(path, r, adamID), candidateIDs(adamID), body)
}

// selectAndProxyCandidates is selectAndProxy with an explicit candidate list
// and routing-cache key.
func selectAndProxyCandidates(w http.ResponseWriter, r *http.Request, path string, adamID string, routeKey string, candidates []string, body []byte) {
	if len(candidates) == 0 {
		WriteLiteError(w, "no available instance")
		return
	}

	query := r.URL.Query()
	queue := append([]string(nil), candidates...)
	tried := make(map[string]bool, len(queue))
	var lastBody []byte
	var lastCode int
	attempts := 0
	probed := false
	deadline := time.Now().Add(overallRetryBudget)

	for len(queue) > 0 && attempts < maxRetryAttempts && time.Now().Before(deadline) {
		id := queue[0]
		queue = queue[1:]
		if tried[id] {
			continue
		}
		tried[id] = true
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
		if !retryable {
			writeLiteBody(w, path, respBody, code)
			return
		}
		lastBody = respBody
		lastCode = code
		log.Infof("%s on instance %s failed (code %d); trying another", path, shortID(id), code)

		// Probe-on-miss: after the first wrong-region/content 404, ask the
		// catalog once, then retry only the instances whose region can serve
		// the item. The probe result is cached for 24h.
		if isProbeOnMissPath(path) && !probed && code == 404 {
			probed = true
			directed, derr := probeCandidates(path, adamID)
			if derr != nil {
				log.Warnf("%s probe failed: %v; falling back to remaining candidates", path, derr)
				continue
			}
			queue = queue[:0]
			for _, d := range directed {
				if !tried[d] {
					queue = append(queue, d)
				}
			}
			if len(queue) == 0 {
				writeLiteBody(w, path, lastBody, lastCode)
				return
			}
		}
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
		selectAndProxy(w, r, path, adamId, nil)
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
