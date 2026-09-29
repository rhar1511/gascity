//go:build linux

package beadspermitbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const childLaunchMode = "--internal-protected-authority-exec-child"

type launchPayload struct {
	Executable  string   `json:"executable"`
	Arguments   []string `json:"arguments"`
	Environment []string `json:"environment"`
}

// LaunchConfig describes the one gc supervisor process this launcher may
// start. It deliberately has no arbitrary command or argument fields.
type LaunchConfig struct {
	AuthorityDirectory  string
	PrivateKeyDirectory string
	GCExecutable        string
	ControllerPath      string
	ControllerUID       uint32
	ControllerGID       uint32
}

// LaunchSupervisor starts one non-root gc supervisor run process. The helper
// PID is registered with a pidfd before it receives the exec payload. The
// broker and launch record live only until that child exits.
func LaunchSupervisor(config LaunchConfig) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "protected Beads authority launcher must run as root") //nolint:errcheck
		return 1
	}
	if config.ControllerUID == 0 || config.ControllerGID == 0 {
		fmt.Fprintln(os.Stderr, "protected Beads controller UID and GID must be nonzero") //nolint:errcheck
		return 1
	}
	gcPath, err := trustedExecutable(config.GCExecutable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validate gc executable: %v\n", err) //nolint:errcheck
		return 1
	}
	broker, err := New(Config{
		AuthorityDirectory: config.AuthorityDirectory, PrivateKeyDirectory: config.PrivateKeyDirectory,
		ControllerGID: config.ControllerGID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start protected Beads authority broker: %v\n", err) //nolint:errcheck
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	serveErrors := make(chan error, 1)
	go func() {
		if serveErr := broker.Serve(ctx); serveErr != nil {
			serveErrors <- serveErr
		}
		close(serveDone)
	}()

	code, launchErr := launchRegisteredChild(broker, config, gcPath, serveErrors)
	cancel()
	closeErr := broker.Close()
	<-serveDone
	var serveErr error
	select {
	case serveErr = <-serveErrors:
	default:
	}
	if launchErr != nil {
		fmt.Fprintf(os.Stderr, "launch protected gc supervisor: %v\n", launchErr) //nolint:errcheck
		return 1
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "close protected Beads authority broker: %v\n", closeErr) //nolint:errcheck
		return 1
	}
	if serveErr != nil {
		fmt.Fprintf(os.Stderr, "protected Beads authority broker stopped: %v\n", serveErr) //nolint:errcheck
		return 1
	}
	return code
}

// ExecChild is the private pre-exec helper entry point used by the launcher.
// It waits for the parent to confirm pidfd registration before exec'ing gc.
func ExecChild() int {
	gate := os.NewFile(3, "protected-authority-launch-gate")
	ready := os.NewFile(4, "protected-authority-launch-ready")
	if gate == nil || ready == nil {
		fmt.Fprintln(os.Stderr, "protected authority launch helper descriptors are unavailable") //nolint:errcheck
		return 1
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		fmt.Fprintf(os.Stderr, "protected authority launch helper readiness failed: %v\n", err) //nolint:errcheck
		return 1
	}
	_ = ready.Close()
	payloadBytes, err := io.ReadAll(io.LimitReader(gate, 1<<20+1))
	_ = gate.Close()
	if err != nil || len(payloadBytes) == 0 || len(payloadBytes) > 1<<20 {
		fmt.Fprintln(os.Stderr, "protected authority launch helper received an invalid exec payload") //nolint:errcheck
		return 1
	}
	var payload launchPayload
	if err := decodeStrictJSON(payloadBytes, &payload); err != nil || !validLaunchPayload(payload) {
		fmt.Fprintln(os.Stderr, "protected authority launch helper rejected the exec payload") //nolint:errcheck
		return 1
	}
	if err := syscall.Exec(payload.Executable, payload.Arguments, payload.Environment); err != nil {
		fmt.Fprintf(os.Stderr, "exec gc supervisor run: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func launchRegisteredChild(broker *Broker, config LaunchConfig, gcPath string, serveErrors <-chan error) (int, error) {
	helperPath, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("find launcher executable: %w", err)
	}
	helperPath, err = filepath.EvalSymlinks(helperPath)
	if err != nil {
		return 1, fmt.Errorf("resolve launcher executable: %w", err)
	}
	if _, err := trustedExecutable(helperPath); err != nil {
		return 1, fmt.Errorf("launcher executable is not protected: %w", err)
	}
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("create launch gate: %w", err)
	}
	defer gateWrite.Close() //nolint:errcheck
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = gateRead.Close()
		return 1, fmt.Errorf("create launch readiness pipe: %w", err)
	}
	defer readyRead.Close() //nolint:errcheck

	cmd := exec.Command(helperPath, childLaunchMode)
	cmd.Env = []string{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{gateRead, readyWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: config.ControllerUID, Gid: config.ControllerGID,
	}}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		_ = gateRead.Close()
		_ = readyWrite.Close()
		return 1, fmt.Errorf("start pre-exec helper: %w", err)
	}
	_ = gateRead.Close()
	_ = readyWrite.Close()
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()
	childStarted := false
	defer func() {
		if !childStarted {
			_ = cmd.Process.Kill()
			<-childDone
		}
	}()
	var ready [1]byte
	if _, err := io.ReadFull(readyRead, ready[:]); err != nil || ready[0] != 1 {
		return 1, errors.New("pre-exec helper did not reach the launch gate")
	}
	arguments := []string{gcPath, "supervisor", "run"}
	if err := broker.RegisterLaunch(cmd.Process.Pid, config.ControllerUID, gcPath, arguments); err != nil {
		return 1, err
	}
	environment, err := controllerEnvironment(config.AuthorityDirectory, config.ControllerUID, config.ControllerPath)
	if err != nil {
		return 1, err
	}
	payload, err := json.Marshal(launchPayload{
		Executable: gcPath, Arguments: arguments, Environment: environment,
	})
	if err != nil || len(payload) > 1<<20 {
		return 1, errors.New("encode controller exec payload failed")
	}
	if _, err := gateWrite.Write(payload); err != nil {
		return 1, fmt.Errorf("release registered controller: %w", err)
	}
	if err := gateWrite.Close(); err != nil {
		return 1, fmt.Errorf("close controller launch gate: %w", err)
	}
	childStarted = true

	for {
		select {
		case err := <-childDone:
			return childExitCode(err), nil
		case sig := <-signals:
			if signalErr := cmd.Process.Signal(sig); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				<-childDone
				return 1, fmt.Errorf("forward signal to controller: %w", signalErr)
			}
		case serveErr := <-serveErrors:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			<-childDone
			return 1, fmt.Errorf("protected key broker failed: %w", serveErr)
		}
	}
}

func childExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

func controllerEnvironment(authorityDirectory string, uid uint32, path string) ([]string, error) {
	account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, fmt.Errorf("look up controller user: %w", err)
	}
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	if !validPathList(path) || !filepath.IsAbs(authorityDirectory) || filepath.Clean(authorityDirectory) != authorityDirectory {
		return nil, errors.New("controller environment paths are invalid")
	}
	return []string{
		"HOME=" + account.HomeDir,
		"LOGNAME=" + account.Username,
		"PATH=" + path,
		"USER=" + account.Username,
		authorityDirectoryEnv + "=" + authorityDirectory,
	}, nil
}

func validLaunchPayload(payload launchPayload) bool {
	return filepath.IsAbs(payload.Executable) && filepath.Clean(payload.Executable) == payload.Executable &&
		len(payload.Arguments) == 3 && payload.Arguments[0] == payload.Executable &&
		payload.Arguments[1] == "supervisor" && payload.Arguments[2] == "run" &&
		validControllerEnvironment(payload.Environment)
}

func validControllerEnvironment(environment []string) bool {
	allowed := map[string]bool{
		"HOME": false, "LOGNAME": false, "PATH": false, "USER": false, authorityDirectoryEnv: false,
	}
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if !ok || !allowedKey(allowed, key) || allowed[key] || value == "" || strings.ContainsRune(value, '\x00') {
			return false
		}
		allowed[key] = true
		values[key] = value
	}
	for _, found := range allowed {
		if !found {
			return false
		}
	}
	return filepath.IsAbs(values["HOME"]) && filepath.Clean(values["HOME"]) == values["HOME"] &&
		values["USER"] == values["LOGNAME"] && validPathList(values["PATH"]) &&
		filepath.IsAbs(values[authorityDirectoryEnv]) && filepath.Clean(values[authorityDirectoryEnv]) == values[authorityDirectoryEnv]
}

func allowedKey(allowed map[string]bool, key string) bool {
	_, ok := allowed[key]
	return ok
}

func validPathList(path string) bool {
	if path == "" {
		return false
	}
	for _, directory := range strings.Split(path, string(os.PathListSeparator)) {
		if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || strings.ContainsRune(directory, '\x00') {
			return false
		}
	}
	return true
}

func trustedExecutable(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("executable path must be clean and absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", errors.New("executable path contains symlinks or is unavailable")
	}
	if err := verifyTrustedPathDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !rootOwned(info) || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("executable is not a protected root-owned regular file")
	}
	return path, nil
}
