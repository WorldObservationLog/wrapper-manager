package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// fetchFromLite performs a request against one lite instance and returns the
// raw response body plus the lite business code. Nothing is written to w, so
// callers may retry on another instance before emitting a response.
func fetchFromLite(inst *WrapperInstance, method, path string, query url.Values, body []byte) (respBody []byte, liteCode int, err error) {
	target := fmt.Sprintf("http://127.0.0.1:%d%s", inst.Port, path)
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	req.Header.Set("Content-Type", "application/json")

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

// writeLiteBody writes a raw lite response body through to the client.
func writeLiteBody(w http.ResponseWriter, respBody []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}

// proxyToLite forwards an HTTP request to the given lite instance and writes
// the lite response through unchanged (used when no retry logic is needed).
func proxyToLite(w http.ResponseWriter, inst *WrapperInstance, method, path string, query url.Values, body []byte) {
	respBody, _, err := fetchFromLite(inst, method, path, query, body)
	if err != nil {
		WriteLiteError(w, fmt.Sprintf("upstream error: %v", err))
		return
	}
	writeLiteBody(w, respBody)
}

// selectAndProxy picks an instance able to serve adamId and proxies the
// request to it. If the first instance fails at the lite business layer
// (e.g. dead account / no asset), it retries once on another available
// instance before returning the failure.
func selectAndProxy(w http.ResponseWriter, r *http.Request, path string, adamID string, body []byte) {
	candidates, err := SelectInstances(adamID)
	if err != nil {
		WriteLiteError(w, err.Error())
		return
	}
	if len(candidates) == 0 {
		WriteLiteError(w, "no available instance")
		return
	}

	query := r.URL.Query()
	var lastBody []byte
	for i, id := range candidates {
		inst := GetInstance(id)
		if inst == nil {
			continue
		}
		respBody, code, ferr := fetchFromLite(inst, r.Method, path, query, body)
		if ferr != nil {
			log.Warnf("%s on instance %s transport error: %v", path, shortID(id), ferr)
			continue
		}
		if code == 0 || i == len(candidates)-1 {
			writeLiteBody(w, respBody)
			return
		}
		// Business failure on a non-final candidate: remember and try another.
		lastBody = respBody
		log.Infof("%s on instance %s failed (code %d); trying another", path, shortID(id), code)
	}
	if lastBody != nil {
		writeLiteBody(w, lastBody)
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
		adamId := r.URL.Query().Get("adamId")
		if adamId == "" {
			WriteLiteError(w, "missing adamId")
			return
		}
		selectAndProxy(w, r, path, adamId, nil)
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
		// Prefer an instance that has lyrics; otherwise fall back to the
		// generic region-candidate selection (with per-instance retry).
		if instID := SelectInstanceForLyrics(adamId, language); instID != "" {
			if inst := GetInstance(instID); inst != nil {
				proxyToLite(w, inst, r.Method, path, r.URL.Query(), nil)
				return
			}
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
	})
}
