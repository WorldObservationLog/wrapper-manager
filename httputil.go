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

// WriteEnvelope writes a unified envelope to the HTTP response.
func WriteEnvelope(w http.ResponseWriter, code int, msg string, data any) {
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
