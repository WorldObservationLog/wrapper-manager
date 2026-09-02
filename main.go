package main

import (
	"flag"
	"fmt"
	log "github.com/sirupsen/logrus"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	PROXY                string
	DeviceInfo           string
	Ready                bool
	ShouldStartInstances int
	readyMu              sync.Mutex
)

func setReady(v bool) {
	readyMu.Lock()
	Ready = v
	readyMu.Unlock()
}

func isReady() bool {
	readyMu.Lock()
	defer readyMu.Unlock()
	return Ready
}

func main() {
	var host = flag.String("host", "localhost", "host of HTTP server")
	var port = flag.Int("port", 8080, "port of HTTP server")
	var mirror = flag.Bool("mirror", false, "use mirror to download wrapper-lite (for Chinese users)")
	var debug = flag.Bool("debug", false, "enable debug output")
	var prepare = flag.Bool("prepare", false, "only download required files")
	flag.StringVar(&PROXY, "proxy", "", "proxy for wrapper and manager")
	flag.StringVar(&DeviceInfo, "device-info", "Music/5.0.2/Android/10/Pixel 8/7663314/en-US/en-US", "device info for wrapper-lite (--device-info pass-through, optional)")
	flag.Parse()

	log.SetOutput(os.Stdout)
	if *debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	// wrapper-lite payload must be present before anything else.
	if !wrapperPayloadReady() {
		log.Warn("wrapper-lite does not exist, downloading...")
		PrepareWrapper(*mirror)
	}

	if *prepare {
		log.Info("prepare done")
		os.Exit(0)
	}

	// Restore persisted instances.
	Instances = make([]*WrapperInstance, 0)
	if _, err := os.Stat("data/instances.json"); err == nil {
		instancesInFile := LoadInstance()
		ShouldStartInstances = len(instancesInFile)
		for _, inst := range instancesInFile {
			restored := &WrapperInstance{
				Id:        inst.Id,
				Region:    inst.Region,
				Port:      inst.Port,
				NoRestart: false,
			}
			InsertInstance(restored)
			go WrapperStart(inst.Id)
		}
	} else {
		ShouldStartInstances = 0
		setReady(true)
	}

	mux := http.NewServeMux()

	// Resource endpoints (wrapper-lite compatible, aggregated over accounts).
	for _, p := range []string{"/status", "/m3u8", "/key", "/lyrics", "/webplayback", "/license"} {
		mux.HandleFunc(p, handleLiteEndpoint)
	}

	// Management endpoints.
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/logout", handleLogout)

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", *host, *port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	log.Infof("wrapperManager running at %s:%d", *host, *port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// handleLogin implements POST /login (two-phase 2FA).
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteLiteError(w, "method not allowed")
		return
	}
	var req LoginRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteLiteError(w, "invalid JSON body")
		return
	}
	if req.Username == "" || req.Password == "" {
		WriteLiteError(w, "missing username or password")
		return
	}

	log.Infof("login request for %s", req.Username)
	result := startLogin(req)
	switch result.Code {
	case 0:
		WriteLiteSuccess(w, map[string]any{"loginId": result.LoginId})
	case 2:
		WriteEnvelope(w, 2, result.Msg, map[string]any{"loginId": result.LoginId})
	default:
		WriteLiteError(w, result.Msg)
	}
}

// handleLogout implements POST /logout.
func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteLiteError(w, "method not allowed")
		return
	}
	var req LogoutRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteLiteError(w, "invalid JSON body")
		return
	}
	if req.Username == "" {
		WriteLiteError(w, "missing username")
		return
	}

	log.Infof("logout request for %s", req.Username)
	if err := startLogout(req.Username); err != nil {
		WriteLiteError(w, err.Error())
		return
	}
	WriteLiteSuccess(w, map[string]any{"username": req.Username})
}
