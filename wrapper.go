package main

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/gofrs/uuid/v5"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// InstanceNamespace is the fixed UUIDv5 namespace used to derive a stable
	// instance id from an account username.
	InstanceNamespace = "77777777-7777-7777-7777-77777777"

	// wrapperDir is where the wrapper-lite payload (rootfs + launchers) lives.
	wrapperDir = "data/wrapper"

	// instanceBaseHost is the base-dir path inside the lite chroot.
	instanceBaseHost = "data/wrapper/rootfs/data/instances"

	// liteReadyTimeout is how long LiteStart waits for /status to report a region.
	liteReadyTimeout = 60 * time.Second
)

// InstanceID returns the deterministic UUIDv5 for an account username.
func InstanceID(username string) string {
	return uuid.NewV5(uuid.FromStringOrNil(InstanceNamespace), username).String()
}

func instanceDir(id string) string {
	return filepath.Join(instanceBaseHost, id)
}

// baseDirArg returns the --base-dir argument passed to wrapper-lite. Because
// the rootless launcher chroots into data/wrapper/rootfs, paths are expressed
// relative to that rootfs.
func baseDirArg(id string) string {
	return "/data/instances/" + id
}

// releaseAssetURL returns the nightly.link URL of the wrapper-lite native
// artifact for the current architecture. The artifact is produced by the
// build-lite workflow on the `lite` branch of WorldObservationLog/wrapper.
func releaseAssetURL() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "https://nightly.link/WorldObservationLog/wrapper/workflows/build-lite/lite/wrapper-lite-linux-x86_64.zip", nil
	case "arm64":
		return "https://nightly.link/WorldObservationLog/wrapper/workflows/build-lite/lite/wrapper-lite-linux-aarch64.zip", nil
	default:
		return "", fmt.Errorf("unsupported arch %s", runtime.GOARCH)
	}
}

// mirrorURL rewrites a download URL through gh-proxy.com for CN users.
func mirrorURL(raw string) string {
	return strings.Replace(raw, "https://nightly.link/", "https://gh-proxy.com/https://nightly.link/", 1)
}

func launcherPath() string {
	return mustAbs(filepath.Join(wrapperDir, "wrapper-lite-rootless"))
}

// absWrapperDir returns the absolute path of the wrapper payload directory
// (the cwd the lite launcher expects, since it chroots into ./rootfs).
func absWrapperDir() string {
	return mustAbs(wrapperDir)
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return abs
}

// wrapperPayloadReady reports whether the wrapper-lite payload is installed.
func wrapperPayloadReady() bool {
	_, err := os.Stat(launcherPath())
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(wrapperDir, "rootfs", "system", "bin", "lite"))
	return err == nil
}

// PrepareWrapper downloads and extracts the wrapper-lite native package when
// missing. mirror routes the download through gh-proxy.com.
func PrepareWrapper(mirror bool) {
	if wrapperPayloadReady() {
		return
	}
	if err := os.MkdirAll(wrapperDir, 0777); err != nil {
		panic(err)
	}

	assetURL, err := releaseAssetURL()
	if err != nil {
		panic(err)
	}
	if mirror {
		assetURL = mirrorURL(assetURL)
	}

	zipPath := filepath.Join("data", fmt.Sprintf("wrapper-lite-%s.zip", runtime.GOARCH))
	log.Warnf("wrapper-lite not present, downloading %s ...", assetURL)

	resp, err := GetHttpClient().Get(assetURL)
	if err != nil {
		panic(fmt.Errorf("failed to download wrapper-lite: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		panic(fmt.Errorf("failed to download wrapper-lite: HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(fmt.Errorf("failed to read wrapper-lite download: %w", err))
	}
	if err = os.WriteFile(zipPath, body, 0777); err != nil {
		panic(err)
	}

	if err := extractZip(zipPath, wrapperDir); err != nil {
		panic(err)
	}
	_ = os.Chmod(launcherPath(), 0777)
	_ = os.Chmod(filepath.Join(wrapperDir, "rootfs", "system", "bin", "lite"), 0777)
	_ = os.Chmod(filepath.Join(wrapperDir, "rootfs", "system", "bin", "linker64"), 0777)
	log.Info("wrapper-lite ready")
}

// extractZip extracts srcZip into dstDir using the standard library (zip
// entries are extracted to paths validated to stay inside dstDir).
func extractZip(srcZip, dstDir string) error {
	zr, err := zip.OpenReader(srcZip)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()

	cleanDst, err := filepath.Abs(dstDir)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		dest := filepath.Join(cleanDst, f.Name)
		if !strings.HasPrefix(dest, cleanDst+string(os.PathSeparator)) && dest != cleanDst {
			return fmt.Errorf("zip entry escapes target: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err = os.MkdirAll(dest, 0777); err != nil {
				return err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(dest), 0777); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0777)
		if err != nil {
			_ = rc.Close()
			return err
		}
		if _, err = io.Copy(out, rc); err != nil {
			_ = out.Close()
			_ = rc.Close()
			return err
		}
		_ = out.Close()
		_ = rc.Close()
	}
	return nil
}

// --- login ---------------------------------------------------------------

// newLiteCmd builds an exec.Cmd for the wrapper-lite-rootless launcher.
// The launcher forks a child that chroots and execs lite; Setpgid makes the
// launcher a process-group leader so KillWrapper can kill the whole group
// (launcher + forked lite) instead of leaving an orphan behind.
func newLiteCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(launcherPath(), args...)
	cmd.Dir = absWrapperDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// startLiteLogin launches the one-shot --login child of wrapper-lite for an
// account and returns immediately. The child's stderr/stdout is drained on
// goroutines; line-based signals drive the login state machine:
//   - "Enter your 2FA code"  -> login2FARequired(id)
//   - child exit + token files -> success/failure resolved by runLoginChild
func startLiteLogin(id, username, password string) (*exec.Cmd, error) {
	dir := instanceDir(id)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, err
	}

	args := []string{
		"--login", fmt.Sprintf("%s:%s", username, password),
		"--code-from-file",
		"--base-dir", baseDirArg(id),
	}
	cmd := newLiteCmd(args...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}

	watchPipe := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			log.Debugf("[lite login %s] %s", shortID(id), line)
			lower := strings.ToLower(line)
			if strings.Contains(lower, "enter your 2fa code") ||
				(strings.Contains(lower, "2fa") && strings.Contains(lower, "code")) {
				login2FARequired(id)
			}
		}
	}
	go watchPipe(stderr)
	go watchPipe(stdout)

	return cmd, nil
}

// --- service mode ---------------------------------------------------------

// liteServiceArgs builds the wrapper-lite service-mode argument list.
func liteServiceArgs(id string, port int) []string {
	args := []string{
		"--base-dir", baseDirArg(id),
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--log-level", "info",
	}
	if PROXY != "" {
		args = append(args, "--proxy", PROXY)
	}
	return args
}

// startLiteService launches (or restarts) the long-running service process for
// an instance and waits until its HTTP /status reports a region.
func startLiteService(instance *WrapperInstance) error {
	if err := os.MkdirAll(instanceDir(instance.Id), 0777); err != nil {
		return err
	}
	cmd := newLiteCmd(liteServiceArgs(instance.Id, instance.Port)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}

	instance.Cmd = cmd
	go logLiteOutput(instance, stdout)
	go logLiteOutput(instance, stderr)

	// Single waiter: resolves exactly once when the process exits.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// Poll /status until a region appears, the process exits, or timeout.
	deadline := time.Now().Add(liteReadyTimeout)
	for {
		if region, err := liteStatusRegion(instance.Port); err == nil && region != "" {
			instance.Region = region
			break
		}
		select {
		case <-exited:
			return fmt.Errorf("lite exited before ready")
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lite did not become ready within %s", liteReadyTimeout)
		}
	}

	// Process exited between readiness and here? Treat as down.
	select {
	case <-exited:
		return fmt.Errorf("lite exited right after becoming ready")
	default:
	}

	// Reap and cascade on exit.
	go func() {
		<-exited
		wrapperDown(instance)
	}()
	return nil
}

// unhealthyOnce guards per-instance unhealthy handling so a burst of failure
// log lines triggers removal only once.
var unhealthyOnce sync.Map // id -> *sync.Once

// logLiteOutput streams a lite instance's stderr/stdout into the manager log
// and watches for account-failure signals. When an account is dead (expired
// subscription, invalid session, ...) the instance is removed and its data
// wiped so it stops being selected for requests (mirrors v1's
// NoSubscriptionHandler).
func logLiteOutput(instance *WrapperInstance, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		log.Infof("[wrapper %s] %s", shortID(instance.Id), line)
		if isAccountFailureSignal(line) {
			once, _ := unhealthyOnce.LoadOrStore(instance.Id, &sync.Once{})
			once.(*sync.Once).Do(func() {
				handleUnhealthyInstance(instance, line)
			})
		}
	}
}

// isAccountFailureSignal reports whether a lite log line indicates the
// account behind this instance has lost its subscription. Mirrors v1, which
// only acted on "No Active Subscription" (other signals such as "end lease"
// are ordinary lifecycle events and must NOT remove an account).
func isAccountFailureSignal(line string) bool {
	return strings.Contains(strings.ToLower(line), "no active subscription")
}

// handleUnhealthyInstance kills the instance, removes it from the registry and
// wipes its account data (so a later /login can re-provision it cleanly).
func handleUnhealthyInstance(instance *WrapperInstance, reason string) {
	log.Warnf("[wrapper %s] account unhealthy (%s); removing instance", shortID(instance.Id), reason)
	instance.NoRestart = true
	_ = KillWrapper(instance.Id)
	RemoveInstance(instance)
	_ = RemoveWrapperDataQuiet(instance.Id)
	SaveInstances()
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// liteStatusRegion queries one lite instance /status and returns its first
// region code ("" when not logged in yet).
func liteStatusRegion(port int) (string, error) {
	body, err := fetchLite(port, http.MethodGet, "/status", nil, nil, "")
	if err != nil {
		return "", err
	}
	var reply LiteReply
	if err = json.Unmarshal(body, &reply); err != nil {
		return "", err
	}
	if reply.Code != 0 {
		return "", fmt.Errorf("status code %d: %s", reply.Code, reply.Msg)
	}
	var data struct {
		Regions []string `json:"regions"`
	}
	if len(reply.Data) > 0 {
		if err = json.Unmarshal(reply.Data, &data); err != nil {
			return "", err
		}
	}
	if len(data.Regions) > 0 {
		return data.Regions[0], nil
	}
	return "", nil
}

// WrapperStart starts a persisted instance (service mode only, no login).
func WrapperStart(id string) {
	instance := GetInstance(id)
	if instance == nil {
		instance = &WrapperInstance{
			Id:        id,
			Port:      GenerateUniquePort(),
			NoRestart: false,
		}
		InsertInstance(instance)
	} else {
		instance.Port = GenerateUniquePort()
		instance.NoRestart = false
	}

	log.Infof("[wrapper %s] starting lite on port %d", shortID(id), instance.Port)
	if err := startLiteService(instance); err != nil {
		log.Warnf("[wrapper %s] start failed: %v", shortID(id), err)
		if !instance.NoRestart {
			go WrapperStart(id)
		}
		return
	}
	log.Infof("[wrapper %s] ready, region=%s", shortID(id), instance.Region)
	// A newly restored/restarted instance becomes ready; mark the manager
	// ready once every persisted instance has come up.
	if countReady() >= ShouldStartInstances {
		setReady(true)
	}
}

// countReady returns how many registered instances are ready (have a region).
func countReady() int {
	n := 0
	for _, inst := range SnapshotInstances() {
		if inst.Region != "" {
			n++
		}
	}
	return n
}

// getInstanceOrNew returns the instance with the given id, creating and
// registering an empty one when absent.
func getInstanceOrNew(id string) *WrapperInstance {
	inst := GetInstance(id)
	if inst != nil {
		return inst
	}
	inst = &WrapperInstance{
		Id:   id,
		Port: GenerateUniquePort(),
	}
	InsertInstance(inst)
	return inst
}

// wrapperDown is triggered when the service process exits.
func wrapperDown(instance *WrapperInstance) {
	log.Infof("[wrapper %s] wrapper down", shortID(instance.Id))
	RemoveInstance(instance)
	if !instance.NoRestart {
		log.Infof("[wrapper %s] restarting", shortID(instance.Id))
		WrapperStart(instance.Id)
	} else {
		SaveInstances()
	}
}

// KillWrapper terminates the whole process group of an instance (the
// wrapper-lite-rootless launcher and the lite child it forks into the chroot).
func KillWrapper(id string) error {
	instance := GetInstance(id)
	if instance == nil {
		return fmt.Errorf("instance %s not found", id)
	}
	if instance.Cmd == nil || instance.Cmd.Process == nil {
		return fmt.Errorf("instance %s process is nil", id)
	}
	pid := instance.Cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the leader when the group does not exist.
		return instance.Cmd.Process.Kill()
	}
	return nil
}

// RemoveWrapperData deletes the on-disk account data directory.
func RemoveWrapperData(id string) {
	err := os.RemoveAll(instanceDir(id))
	if err != nil {
		panic(err)
	}
}
