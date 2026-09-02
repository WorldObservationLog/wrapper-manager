package main

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// LoginResult describes the outcome of a login attempt for the HTTP layer.
type LoginResult struct {
	Code    int // 0 = success, 2 = 2FA required, -1 = failure
	Msg     string
	LoginId string
}

// LoginRequest is the JSON body of POST /login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// LogoutRequest is the JSON body of POST /logout.
type LogoutRequest struct {
	Username string `json:"username"`
}

// LoginState captures the progress of an in-flight account login.
type LoginState struct {
	Id       string
	Username string

	mu          sync.Mutex
	pending2FA  bool
	reported2FA bool
	cmd         *exec.Cmd

	done chan error // nil => success; error => failure
}

// loginRegistry tracks in-flight logins keyed by instance id.
var loginRegistry = struct {
	sync.Mutex
	m map[string]*LoginState
}{m: make(map[string]*LoginState)}

func registerLogin(id string) *LoginState {
	st := &LoginState{
		Id:       id,
		Username: "",
		done:     make(chan error, 1),
	}
	loginRegistry.Lock()
	loginRegistry.m[id] = st
	loginRegistry.Unlock()
	return st
}

func setLoginUsername(id, username string) {
	loginRegistry.Lock()
	defer loginRegistry.Unlock()
	if st, ok := loginRegistry.m[id]; ok {
		st.Username = username
	}
}

func getLogin(id string) *LoginState {
	loginRegistry.Lock()
	defer loginRegistry.Unlock()
	return loginRegistry.m[id]
}

func removeLogin(id string) {
	loginRegistry.Lock()
	delete(loginRegistry.m, id)
	loginRegistry.Unlock()
}

// hasTokenFiles reports whether a successful login artifact exists in the
// instance data dir (lite writes STOREFRONT_ID and MUSIC_TOKEN there).
func hasTokenFiles(id string) bool {
	for _, name := range []string{"STOREFRONT_ID", "MUSIC_TOKEN"} {
		if _, err := os.Stat(instanceDir(id) + "/" + name); err != nil {
			return false
		}
	}
	return true
}

// startLogin begins (or continues) a login for an account.
//
// When req.Code == "" and no login is in flight, it launches the one-shot
// lite --login child and returns as soon as the outcome is known:
//   - code 0: tokens cached, service instance started (or starting);
//   - code 2: lite requires a 2FA code; the caller should retry with req.Code;
//   - code -1: login failed (bad credentials, no subscription, timeout).
//
// When req.Code != "", the code is written to 2fa.txt and the pending child
// finishes the login; this call blocks (polling) until the child exits.
func startLogin(req LoginRequest) LoginResult {
	id := InstanceID(req.Username)

	// Same account already registered as a ready instance -> already login.
	if inst := GetInstance(id); inst != nil && inst.Region != "" {
		return LoginResult{Code: -1, Msg: "already login", LoginId: id}
	}

	// If a login for this account is pending 2FA, just feed the code.
	if req.Code != "" {
		if st := getLogin(id); st != nil && st.isPending2FA() {
			if err := provide2FACode(id, req.Code); err != nil {
				return LoginResult{Code: -1, Msg: err.Error(), LoginId: id}
			}
			return waitForLoginFinish(id, st)
		}
		return LoginResult{Code: -1, Msg: "no pending login for this account, restart login without code", LoginId: id}
	}

	// No code: (re)start from scratch.
	st := registerLogin(id)
	setLoginUsername(id, req.Username)
	// Clean stale data from a previous failed attempt.
	_ = RemoveWrapperDataQuiet(id)

	go runLoginChild(id, req.Username, req.Password, st)
	return waitForLoginFinish(id, st)
}

// runLoginChild launches the one-shot lite --login child, monitors its exit,
// and drives the state machine to completion.
func runLoginChild(id, username, password string, st *LoginState) {
	log.Infof("[wrapper %s] login started for %s", shortID(id), username)
	cmd, err := startLiteLogin(id, username, password)
	if err != nil {
		loginFailed(id, st, fmt.Errorf("failed to start login process: %w", err))
		return
	}
	st.mu.Lock()
	st.cmd = cmd
	st.mu.Unlock()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// Poll for 2FA requirement while the child runs.
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case werr := <-waitCh:
			// Child exited. Token files present => success; else classify.
			if hasTokenFiles(id) {
				finishLoginSuccess(id, st)
				return
			}
			cause := classifyLoginFailure(werr)
			loginFailed(id, st, cause)
			return
		case <-ticker.C:
			if st.isPending2FA() {
				// 2FA announced; leave the child running (it polls 2fa.txt)
				// and keep waiting for its exit. The HTTP layer will already
				// have returned code 2 to the client; nothing else to do.
				continue
			}
		}
	}
}

// classifyLoginFailure maps a child exit to a human-readable cause.
func classifyLoginFailure(werr error) error {
	exitCode := -1
	if werr != nil {
		var ee interface{ ExitCode() int }
		if errors.As(werr, &ee) {
			exitCode = ee.ExitCode()
		}
	}
	switch exitCode {
	case 0:
		return errors.New("login process exited without caching tokens")
	default:
		return fmt.Errorf("login failed (exit code %d)", exitCode)
	}
}

// waitForLoginFinish blocks until the in-flight login resolves: the child
// exits (success/failure), or 2FA becomes required. When 2FA is required it
// is reported once (code 2); a second call (the client resubmits with a code)
// blocks until the child exits with the final outcome.
func waitForLoginFinish(id string, st *LoginState) LoginResult {
	timeout := time.NewTimer(150 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case err := <-st.done:
			if err == nil {
				return LoginResult{Code: 0, Msg: "SUCCESS", LoginId: id}
			}
			return LoginResult{Code: -1, Msg: err.Error(), LoginId: id}
		case <-timeout.C:
			return LoginResult{Code: -1, Msg: "login timeout", LoginId: id}
		case <-time.After(200 * time.Millisecond):
			if st.claimPending2FA() {
				return LoginResult{Code: 2, Msg: "2fa code require", LoginId: id}
			}
		}
	}
}

func (s *LoginState) isPending2FA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending2FA
}

// claimPending2FA reports whether the login just became 2FA-pending and has
// not yet been reported to a client. It returns true exactly once per login.
func (s *LoginState) claimPending2FA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending2FA && !s.reported2FA {
		s.reported2FA = true
		return true
	}
	return false
}

// login2FARequired marks the login as awaiting a 2FA code.
func login2FARequired(id string) {
	st := getLogin(id)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.pending2FA = true
	st.mu.Unlock()
	log.Infof("[wrapper %s] 2FA code required", shortID(id))
}

// provide2FACode writes the user-supplied code into the instance 2fa.txt,
// which the waiting lite login child polls.
func provide2FACode(id string, code string) error {
	dir := instanceDir(id)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	path := dir + "/2fa.txt"
	return os.WriteFile(path, []byte(strings.TrimSpace(code)), 0777)
}

// finishLoginSuccess is called when tokens are on disk: it starts the service
// process and, once the instance reports ready, resolves the login.
func finishLoginSuccess(id string, st *LoginState) {
	go func() {
		instance := getInstanceOrNew(id)
		instance.NoRestart = false
		if err := startLiteService(instance); err != nil {
			log.Errorf("[wrapper %s] failed to start after login: %v", shortID(id), err)
			loginFailed(id, st, fmt.Errorf("failed to start service: %w", err))
			return
		}
		if countReady() >= ShouldStartInstances {
			setReady(true)
		}
		SaveInstances()
		resolveLogin(id, st, nil)
	}()
}

// resolveLogin completes the pending login.
func resolveLogin(id string, st *LoginState, cause error) {
	removeLogin(id)
	select {
	case st.done <- cause:
	default:
	}
}

// loginFailed cleans up the instance data and resolves the pending login with
// an error.
func loginFailed(id string, st *LoginState, cause error) {
	log.Warnf("[wrapper %s] login failed: %v", shortID(id), cause)
	_ = RemoveWrapperDataQuiet(id)
	resolveLogin(id, st, cause)
}

// RemoveWrapperDataQuiet removes an instance data dir without panicking.
func RemoveWrapperDataQuiet(id string) error {
	if err := os.RemoveAll(instanceDir(id)); err != nil {
		return err
	}
	return nil
}

// startLogout terminates and removes an account instance.
func startLogout(username string) error {
	id := InstanceID(username)
	instance := GetInstance(id)
	if instance == nil {
		return errors.New("no such account")
	}
	instance.NoRestart = true
	if err := KillWrapper(id); err != nil {
		return fmt.Errorf("failed to kill wrapper: %w", err)
	}
	RemoveWrapperData(id)
	SaveInstances()
	log.Infof("[wrapper %s] logged out %s", shortID(id), username)
	return nil
}
