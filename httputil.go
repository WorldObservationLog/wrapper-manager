package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// decodeJSON decodes a JSON request body into v.
func decodeJSON(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	return dec.Decode(v)
}

// httpClientSingleton is a process-wide http.Client with a reused transport
// (connection pooling). Creating a client/transport per call defeats keep-alive
// and kills throughput, which matters at this request rate.
var (
	httpClientOnce   sync.Once
	httpClientShared *http.Client
)

// GetHttpClient returns the shared http.Client honoring the global PROXY
// setting. The transport is built once and reused: connections to the local
// wrapper-lite instances and to Apple are kept alive and pooled.
func GetHttpClient() *http.Client {
	httpClientOnce.Do(func() {
		tr := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		if PROXY != "" {
			proxyUrl, err := url.Parse(PROXY)
			if err == nil {
				tr.Proxy = http.ProxyURL(proxyUrl)
			}
		}
		httpClientShared = &http.Client{
			Transport: tr,
			Timeout:   60 * time.Second,
		}
	})
	return httpClientShared
}

// LiteReply is the wire envelope used by wrapper-lite and by wrapper-manager:
// {"code":0,"msg":"SUCCESS","data":{...}}.
type LiteReply struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Cache-Control headers used to steer Cloudflare's cache for the proxied
// wrapper-lite API. Success (code 0) on cacheable endpoints may be cached;
// anything else must NOT be cached (a cached failure would poison the edge
// cache until the TTL expires).
const (
	headerCacheControl = "Cache-Control"

	// cacheableTTLSeconds is a reference max-age on cacheable successes. When
	// Cloudflare is configured with "ignore origin Cache-Control and use this
	// TTL", the edge TTL wins; otherwise this value is used.
	cacheableTTLSeconds = 3600

	cacheControlPublic  = "public, max-age=3600"
	cacheControlNoStore = "no-store"
)

// setCacheHeader sets Cache-Control based on whether the endpoint is cacheable
// (part of the media/catalog API surface) and whether the business code is a
// success (0).
func setCacheHeader(w http.ResponseWriter, cacheableEndpoint bool, code int) {
	if !cacheableEndpoint {
		w.Header().Set(headerCacheControl, cacheControlNoStore)
		return
	}
	if code == 0 {
		w.Header().Set(headerCacheControl, cacheControlPublic)
	} else {
		w.Header().Set(headerCacheControl, cacheControlNoStore)
	}
}

// WriteEnvelope writes a unified envelope to the HTTP response. cacheable
// marks endpoints that Cloudflare is allowed to cache on success.
func WriteEnvelope(w http.ResponseWriter, code int, msg string, data any) {
	setCacheHeader(w, false, code)
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"code":-1,"msg":"internal error","data":null}`)
	}
	_, _ = w.Write(body)
}

// WriteEnvelopeCacheable is like WriteEnvelope but marks the endpoint as
// cacheable, so successful (code 0) responses get a public Cache-Control and
// failures are still no-store.
func WriteEnvelopeCacheable(w http.ResponseWriter, code int, msg string, data any) {
	setCacheHeader(w, true, code)
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"code":-1,"msg":"internal error","data":null}`)
	}
	_, _ = w.Write(body)
}

// WriteLiteSuccess writes code:0 with the given data object.
func WriteLiteSuccess(w http.ResponseWriter, data any) {
	WriteEnvelope(w, 0, "SUCCESS", data)
}

// WriteLiteError writes a non-zero envelope error.
func WriteLiteError(w http.ResponseWriter, msg string) {
	WriteEnvelope(w, -1, msg, nil)
}

// fetchLite performs a request against a running wrapper-lite instance and
// returns the raw body. The response envelope is passed through untouched so
// that clients see exactly the lite contract.
func fetchLite(port int, method, path string, query url.Values, body io.Reader, contentType string) ([]byte, error) {
	u := url.URL{
		Scheme:   "http",
		Host:     fmt.Sprintf("127.0.0.1:%d", port),
		Path:     path,
		RawQuery: query.Encode(),
	}
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}
