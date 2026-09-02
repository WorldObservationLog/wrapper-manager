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

// proxyToLite forwards an HTTP request to the given lite instance and writes
// the lite response (status + body) through unchanged.
func proxyToLite(w http.ResponseWriter, inst *WrapperInstance, method, path string, query url.Values, body []byte) {
	target := fmt.Sprintf("http://127.0.0.1:%d%s", inst.Port, path)
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		WriteLiteError(w, err.Error())
		return
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := GetHttpClient().Do(req)
	if err != nil {
		WriteLiteError(w, fmt.Sprintf("upstream error: %v", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		WriteLiteError(w, fmt.Sprintf("upstream read error: %v", err))
		return
	}
	// Pass through the lite envelope untouched.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// selectAndProxy picks an instance able to serve adamId and proxies the
// request to it. body carries the raw request body for POST endpoints
// (nil for GETs).
func selectAndProxy(w http.ResponseWriter, r *http.Request, path string, adamID string, body []byte) {
	instID, err := SelectInstance(adamID)
	if err != nil {
		WriteLiteError(w, err.Error())
		return
	}
	if instID == "" {
		WriteLiteError(w, "no available instance")
		return
	}
	inst := GetInstance(instID)
	if inst == nil {
		WriteLiteError(w, "no available instance")
		return
	}
	proxyToLite(w, inst, r.Method, path, r.URL.Query(), body)
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
		// Prefer an instance that has lyrics; fall back to region selection.
		language := r.URL.Query().Get("language")
		if language == "" {
			language = "en"
		}
		instID := SelectInstanceForLyrics(adamId, language)
		if instID == "" {
			var err error
			instID, err = SelectInstance(adamId)
			if err != nil {
				WriteLiteError(w, err.Error())
				return
			}
		}
		if instID == "" {
			WriteLiteError(w, "no available instance")
			return
		}
		inst := GetInstance(instID)
		if inst == nil {
			WriteLiteError(w, "no available instance")
			return
		}
		proxyToLite(w, inst, r.Method, path, r.URL.Query(), nil)
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
