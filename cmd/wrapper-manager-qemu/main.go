// Command wrapper-manager-qemu boots wrapper-manager inside a QEMU Linux guest
// and forwards the manager's HTTP port to the host, so wrapper-manager runs on
// Windows / macOS / Linux alike (the manager itself only needs Linux, which the
// guest provides).
//
// Modeled after upstream wrapper-lite-qemu (WorldObservationLog/wrapper):
// the launcher locates qemu-system-x86_64, boots the bundled kernel + initrd,
// attaches a persistent data.img and forwards hostPort -> guest 8080.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// ---- defaults (mirror upstream wrapper-lite-qemu) ----

var (
	gHost      = "127.0.0.1" // host address the forwarded port binds to
	gHostPort  = "8080"      // host port (manager HTTP)
	gGuestPort = "8080"      // guest manager port
	gMemory    = "1024"      // guest memory in MB
	gSmp       = "2"         // guest CPU count
	gAccel     = ""          // forced accel (kvm|hvf|whpx|tcg)
	gQemuBin   = ""          // explicit qemu binary
	gDataDir   = ""          // persistent dir holding data.img (default: ~/.wrapper-manager)
	gKernel    = ""          // explicit vmlinuz path
	gInitrd    = ""          // explicit initramfs path
	gAssetsDir = ""          // asset dir override (default: next to executable)
)

const (
	guestKernelName = "vmlinuz-lite-qemu"
	guestInitrdName = "wrapper-manager-initramfs.cpio.gz"
	dataImageName   = "data.img"
)

func getenv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// ---- path helpers ----

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// executableDir returns the directory of the current executable.
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// assetDir returns the directory that holds the guest images. Defaults to
// <exe-dir>/guest; overridable with --assets-dir.
func assetDir() string {
	if gAssetsDir != "" {
		return gAssetsDir
	}
	return filepath.Join(executableDir(), "guest")
}

// dataDir returns the persistent directory holding data.img.
func dataDir() string {
	if gDataDir != "" {
		return gDataDir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return assetDir()
	}
	return filepath.Join(home, ".wrapper-manager")
}

// ---- qemu discovery ----

func qemuName() string {
	if runtime.GOOS == "windows" {
		return "qemu-system-x86_64.exe"
	}
	return "qemu-system-x86_64"
}

// findOnPath searches PATH for name.
func findOnPath(name string) (string, bool) {
	path := os.Getenv("PATH")
	sep := string(os.PathListSeparator)
	for _, dir := range strings.Split(path, sep) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if fileExists(cand) {
			return cand, true
		}
	}
	return "", false
}

// locateQemu resolves the qemu binary: -qemu-bin > QEMU_BIN > PATH > bundled.
func locateQemu() (string, error) {
	if gQemuBin != "" {
		if !fileExists(gQemuBin) {
			return "", fmt.Errorf("qemu binary not found: %s", gQemuBin)
		}
		return gQemuBin, nil
	}
	if v := getenv("QEMU_BIN", ""); v != "" {
		if !fileExists(v) {
			return "", fmt.Errorf("QEMU_BIN not found: %s", v)
		}
		return v, nil
	}
	name := qemuName()
	if p, ok := findOnPath(name); ok {
		return p, nil
	}
	// Bundled: <assets>/qemu/bin/<name>
	bundled := filepath.Join(assetDir(), "qemu", "bin", name)
	if fileExists(bundled) {
		return bundled, nil
	}
	return "", fmt.Errorf("qemu-system-x86_64 not found on PATH nor bundled in %s (set QEMU_BIN or install QEMU)", assetDir())
}

// ---- acceleration ----

func canUseKvm() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// autoAccel picks the best accelerator for the host platform.
func autoAccel() string {
	switch runtime.GOOS {
	case "darwin":
		// HVF on Apple Silicon cannot accelerate x86_64 guests; use TCG.
		return "tcg"
	case "windows":
		return "whpx"
	default:
		if canUseKvm() {
			return "kvm"
		}
		return "tcg"
	}
}

// ---- guest images ----

func guestImagePaths() (kernel, initrd, dataImg string, err error) {
	dir := assetDir()
	kernel = filepath.Join(dir, guestKernelName)
	initrd = filepath.Join(dir, guestInitrdName)
	dataImg = filepath.Join(dataDir(), dataImageName)
	if gKernel != "" {
		kernel = gKernel
	}
	if gInitrd != "" {
		initrd = gInitrd
	}
	missing := []string{}
	if !fileExists(kernel) {
		missing = append(missing, kernel)
	}
	if !fileExists(initrd) {
		missing = append(missing, initrd)
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("missing guest images:\n  %s\n(download from nightly.link or pass --assets-dir)", strings.Join(missing, "\n  "))
	}
	if !fileExists(dataImg) {
		fmt.Fprintf(os.Stderr, "[init] creating empty data image at %s\n", dataImg)
		if err := createEmptyDataImage(dataImg); err != nil {
			return "", "", "", fmt.Errorf("failed to create data image: %w", err)
		}
	}
	return kernel, initrd, dataImg, nil
}

// createEmptyDataImage creates a 512MB ext4 image when missing. It prefers
// mke2fs (Linux); falls back to qemu-img (ships with QEMU on every platform)
// which can create a raw image that the guest formats on first boot (the
// guest init runs mkfs.ext4 when mounting fails).
func createEmptyDataImage(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(512 * 1024 * 1024); err != nil {
		f.Close()
		return err
	}
	f.Close()

	if mke2fs, err := exec.LookPath("mke2fs"); err == nil {
		out, err := exec.Command(mke2fs, "-q", "-t", "ext4", "-F", path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mke2fs failed: %v: %s", err, string(out))
		}
		return nil
	}
	// No mke2fs (macOS/Windows hosts): the raw image will be formatted by the
	// guest init (it runs mkfs.ext4 from the initramfs busybox on mount
	// failure). Just confirm the file is a valid raw image of the right size.
	fmt.Fprintln(os.Stderr, "[init] mke2fs not found; guest will format the data image on first boot")
	return nil
}

// ---- qemu invocation ----

func buildQemuArgs(qemuBin, kernel, initrd, dataImg string) []string {
	accel := gAccel
	if accel == "" {
		accel = autoAccel()
	}
	dir := assetDir()

	args := []string{qemuBin}
	// Point QEMU at bundled firmware when present.
	if fileExists(filepath.Join(dir, "qemu", "bin", "bios-256k.bin")) {
		args = append(args, "-L", filepath.Join(dir, "qemu", "bin"))
	}
	args = append(args, "-accel")
	if accel == "whpx" {
		args = append(args, "whpx,kernel-irqchip=off")
	} else {
		args = append(args, accel)
	}
	switch accel {
	case "kvm":
		args = append(args, "-cpu", "host")
	case "whpx":
		args = append(args, "-cpu", "qemu64-v1")
	default:
		args = append(args, "-cpu", "max")
	}
	args = append(args,
		"-m", gMemory,
		"-smp", gSmp,
		"-kernel", kernel,
		"-initrd", initrd,
		"-append", "console=ttyS0 quiet net.ifnames=0 biosdevname=0",
		"-display", "none",
		"-serial", "stdio",
		"-no-reboot",
		"-nic", fmt.Sprintf("user,model=e1000,hostfwd=tcp:%s:%s-:%s", gHost, gHostPort, gGuestPort),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", dataImg),
	)
	return args
}

// runQemu launches qemu and blocks until it exits, forwarding signals.
// qemu's stderr is captured (and echoed) so accelerator init failures can be
// detected for a TCG fallback.
func runQemu(args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var stderrBuf strings.Builder
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			stderrBuf.WriteString(line + "\n")
			fmt.Fprintln(os.Stderr, line)
		}
	}()

	if err := cmd.Start(); err != nil {
		return err
	}

	// Forward SIGINT/SIGTERM to qemu so the guest can shut down cleanly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n[run] received interrupt, stopping qemu...")
		_ = cmd.Process.Signal(os.Interrupt)
	}()

	err = cmd.Wait()
	signal.Stop(sigCh)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 0 {
				return nil // clean exit (guest powered off)
			}
			// Immediate non-zero exit (accel init failure, bad args, ...)
			return &qemuExitError{code: ee.ExitCode(), stderr: stderrBuf.String()}
		}
		return err
	}
	return nil
}

// qemuExitError carries the qemu exit code and captured stderr.
type qemuExitError struct {
	code   int
	stderr string
}

func (e *qemuExitError) Error() string {
	return fmt.Sprintf("qemu exited with code %d: %s", e.code, e.stderr)
}

// ---- main ----

func usage() {
	fmt.Fprintf(os.Stderr, `wrapper-manager-qemu - run wrapper-manager inside a QEMU guest

Usage: wrapper-manager-qemu [options]

Options:
  --host <addr>      host address the forwarded port binds to (default 127.0.0.1; use 0.0.0.0 to expose)
  --host-port <port> host port (default 8080)
  --guest-port <port> guest manager port (default 8080; must match manager -port in the guest)
  --memory <MB>      guest memory (default 1024)
  --smp <N>          guest CPUs (default 2)
  --accel <accel>    force acceleration: kvm|hvf|whpx|tcg (default auto)
  --qemu-bin <path>  qemu binary (default: QEMU_BIN > PATH > bundled)
  --assets-dir <dir> directory holding vmlinuz-lite-qemu + wrapper-manager-initramfs.cpio.gz
                     (default: <exe>/guest)
  --data-dir <dir>   persistent directory holding data.img (default: ~/.wrapper-manager)
  --kernel <path>    explicit kernel path (overrides --assets-dir)
  --initrd <path>    explicit initramfs path (overrides --assets-dir)
  -h, --help         show this help

Environment fallbacks: QEMU_BIN, HOST_PORT, GUEST_PORT, MEMORY, SMP
`)
}

func main() {
	gHost = getenv("LITE_QEMU_HOST", gHost)
	gHostPort = getenv("HOST_PORT", gHostPort)
	gGuestPort = getenv("GUEST_PORT", gGuestPort)
	gMemory = getenv("MEMORY", gMemory)
	gSmp = getenv("SMP", gSmp)
	gAccel = getenv("LITE_QEMU_ACCEL", "")
	gQemuBin = getenv("QEMU_BIN", "")

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--host":
			gHost = next()
		case "--host-port":
			gHostPort = next()
		case "--guest-port":
			gGuestPort = next()
		case "--memory":
			gMemory = next()
		case "--smp":
			gSmp = next()
		case "--accel":
			gAccel = next()
		case "--qemu-bin":
			gQemuBin = next()
		case "--assets-dir":
			gAssetsDir = next()
		case "--data-dir":
			gDataDir = next()
		case "--kernel":
			gKernel = next()
		case "--initrd":
			gInitrd = next()
		case "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "[run] unknown argument: %s\n", a)
			usage()
			os.Exit(1)
		}
	}

	qemuBin, err := locateQemu()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[run] "+err.Error())
		os.Exit(1)
	}
	kernel, initrd, dataImg, err := guestImagePaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[run] "+err.Error())
		os.Exit(1)
	}

	accel := gAccel
	if accel == "" {
		accel = autoAccel()
	}
	fmt.Fprintf(os.Stderr, "[run] starting wrapper-manager guest (host %s:%s -> guest %s, mem %sMB, accel %s)\n",
		gHost, gHostPort, gGuestPort, gMemory, accel)
	fmt.Fprintf(os.Stderr, "[run] qemu=%s\n", qemuBin)
	fmt.Fprintf(os.Stderr, "[run] kernel=%s\n", kernel)
	fmt.Fprintf(os.Stderr, "[run] initrd=%s\n", initrd)
	fmt.Fprintf(os.Stderr, "[run] data=%s\n", dataImg)

	args2 := buildQemuArgs(qemuBin, kernel, initrd, dataImg)
	rc := runQemu(args2)
	if rc != nil {
		// If acceleration failed to initialize (e.g. KVM permission denied)
		// and the user did not force an accel, fall back to TCG.
		if gAccel == "" && accel != "tcg" && isAccelInitError(rc) {
			fmt.Fprintf(os.Stderr, "[run] %s unavailable (%v), falling back to tcg\n", accel, rc)
			oldAccel := gAccel
			gAccel = "tcg"
			args3 := buildQemuArgs(qemuBin, kernel, initrd, dataImg)
			gAccel = oldAccel
			rc = runQemu(args3)
		}
	}
	if rc != nil {
		fmt.Fprintln(os.Stderr, "[run] qemu error:", rc)
		os.Exit(1)
	}
}

// isAccelInitError reports whether a qemu failure looks like an accelerator
// initialization problem (stderr contains "failed to initialize" or "Could
// not access").
func isAccelInitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to initialize") ||
		strings.Contains(msg, "Could not access") ||
		strings.Contains(msg, "invalid accelerator") ||
		strings.Contains(msg, "no accelerator") ||
		strings.Contains(msg, "Permission denied")
}
