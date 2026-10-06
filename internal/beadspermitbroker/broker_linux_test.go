//go:build linux

package beadspermitbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestRegisteredPIDIsBoundToPolicyAndHandle(t *testing.T) {
	uid := uint32(os.Getuid())
	executable, arguments := currentProcessCommand(t)
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfd_open is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(pidfd) })
	digest := sha256.Sum256([]byte("host policy"))
	key := []byte("private key bytes")
	broker := &Broker{
		digest: digest,
		keys:   map[string][]byte{"allowed": key},
		record: &launchRecord{
			pid: os.Getpid(), uid: uid, pidfd: pidfd, executable: executable, arguments: arguments, digest: digest,
			handles: map[string]struct{}{"allowed": {}},
		},
	}
	fd, err := broker.authorizedKeyFD(os.Getpid(), uid, digest, "allowed")
	if err != nil {
		t.Fatalf("registered controller was rejected: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
	if err != nil || seals&privateKeyMemfdSeals != privateKeyMemfdSeals {
		t.Fatalf("returned key descriptor seals = %#x, %v", seals, err)
	}
	got := make([]byte, len(key))
	n, err := unix.Pread(fd, got, 0)
	if err != nil || n != len(got) {
		t.Fatalf("read returned key descriptor: bytes=%d err=%v", n, err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("returned key contents = %q, want test key", got)
	}
	zero(got)

	wrongDigest := sha256.Sum256([]byte("other policy"))
	for _, test := range []struct {
		name   string
		pid    int
		uid    uint32
		digest [sha256.Size]byte
		handle string
	}{
		{name: "unregistered pid", pid: os.Getpid() + 1, uid: uid, digest: digest, handle: "allowed"},
		{name: "wrong uid", pid: os.Getpid(), uid: uid + 1, digest: digest, handle: "allowed"},
		{name: "wrong policy digest", pid: os.Getpid(), uid: uid, digest: wrongDigest, handle: "allowed"},
		{name: "unlisted key handle", pid: os.Getpid(), uid: uid, digest: digest, handle: "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if unexpectedFD, err := broker.authorizedKeyFD(test.pid, test.uid, test.digest, test.handle); err == nil {
				_ = unix.Close(unexpectedFD)
				t.Fatal("unbound broker request was authorized")
			}
		})
	}
}

func TestRegisterLaunchRejectsRootAndSecondController(t *testing.T) {
	broker := &Broker{keys: map[string][]byte{"allowed": []byte("key")}}
	executable, arguments := currentProcessCommand(t)
	if err := broker.RegisterLaunch(os.Getpid(), 0, executable, arguments); err == nil {
		t.Fatal("root controller identity was accepted")
	}
	uid := uint32(os.Getuid())
	if uid != 0 {
		if err := broker.RegisterLaunch(os.Getpid(), uid, executable, arguments); err != nil {
			t.Fatalf("register current process: %v", err)
		}
		if err := broker.RegisterLaunch(os.Getpid(), uid, executable, arguments); err == nil {
			t.Fatal("second controller launch was accepted")
		}
		if broker.record != nil {
			t.Cleanup(func() { _ = unix.Close(broker.record.pidfd) })
		}
	}
}

func TestLaunchPayloadIsExactlySupervisorRun(t *testing.T) {
	valid := launchPayload{
		Executable:  "/usr/bin/gc",
		Arguments:   []string{"/usr/bin/gc", "supervisor", "run"},
		Environment: []string{"HOME=/home/controller", "LOGNAME=controller", "PATH=/usr/bin:/bin", "USER=controller", authorityDirectoryEnv + "=/etc/gc/protected-authority"},
	}
	if !validLaunchPayload(valid) {
		t.Fatal("valid supervisor launch payload was rejected")
	}
	valid.Arguments[2] = "start"
	if validLaunchPayload(valid) {
		t.Fatal("non-run gc command was accepted by launch helper")
	}
	valid.Arguments[2] = "run"
	valid.Environment = append(valid.Environment, "AWS_SECRET_ACCESS_KEY=secret")
	if !validLaunchPayload(valid) {
		t.Fatal("curated provider credential was rejected by launch helper")
	}
	valid.Environment = append(valid.Environment, "LD_PRELOAD=/tmp/inject.so")
	if validLaunchPayload(valid) {
		t.Fatal("loader injection variable was accepted by launch helper")
	}
	valid.Environment = valid.Environment[:len(valid.Environment)-1]
	valid.Environment = append(valid.Environment, "PATH=/bin")
	if validLaunchPayload(valid) {
		t.Fatal("duplicate environment variable was accepted by launch helper")
	}
}

func TestControllerEnvironmentExcludesLauncherSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "root-launcher-secret")
	t.Setenv("GC_BEADS_PROTECTED_KEY_TOKEN", "root-private-key-secret")
	environment, err := controllerEnvironment("/etc/gc/protected-authority", uint32(os.Getuid()), "/usr/local/bin:/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "root-launcher-secret") || strings.Contains(joined, "root-private-key-secret") || len(environment) != 5 {
		t.Fatalf("controller environment contains launcher state: %q", joined)
	}
	for _, key := range []string{"HOME=", "USER=", "LOGNAME=", "PATH=", authorityDirectoryEnv + "="} {
		found := false
		for _, item := range environment {
			found = found || strings.HasPrefix(item, key)
		}
		if !found {
			t.Errorf("controller environment is missing %q", key)
		}
	}
}

func TestControllerEnvironmentFileAllowsExplicitSupervisorInputs(t *testing.T) {
	data := []byte(`{
		"CLAUDE_CONFIG_DIR":"/srv/controller/claude",
		"CLAUDE_CODE_OAUTH_TOKEN":"claude-oauth-secret",
		"CLAUDE_CODE_SUBAGENT_MODEL":"sonnet",
		"CLAUDE_CODE_EFFORT_LEVEL":"high",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1",
		"GC_HOME":"/srv/controller/gc-home",
		"XDG_RUNTIME_DIR":"/run/user/1001",
		"GC_COMPATIBILITY_AUTHORITY_DIR":"/etc/gc/compatibility-authority",
		"GC_COMPATIBILITY_AUTHORITY_MAX_REVOCATION_AGE":"15m",
		"GC_PR_HUMAN_TRUST":"trusted-reviewer",
		"ANTHROPIC_API_KEY":"provider-secret",
		"GC_DOLT_HOST":"dolt.example.internal",
		"GC_DOLT_LOGLEVEL":"warn",
		"T3_WS_URL":"ws://127.0.0.1:4100/ws",
		"TZ":"America/New_York",
		"LANG":"C.UTF-8"
	}`)
	extras, err := parseControllerEnvironment(data)
	if err != nil {
		t.Fatalf("parse explicit controller environment: %v", err)
	}
	environment, err := controllerEnvironmentWithExtras("/etc/gc/protected-authority", uint32(os.Getuid()), "/usr/local/bin:/usr/bin:/bin", extras)
	if err != nil {
		t.Fatalf("build explicit controller environment: %v", err)
	}
	if !validControllerEnvironment(environment) {
		t.Fatalf("explicit controller environment failed child validation: %#v", environment)
	}
	if !validLaunchPayload(launchPayload{
		Executable:  "/usr/bin/gc",
		Arguments:   []string{"/usr/bin/gc", "supervisor", "run"},
		Environment: environment,
	}) {
		t.Fatal("child rejected the final explicit supervisor launch payload")
	}
	if len(environment) < 5 || !sort.StringsAreSorted(environment[5:]) {
		t.Fatalf("extra controller environment entries are not sorted: %#v", environment[5:])
	}
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		"CLAUDE_CONFIG_DIR=/srv/controller/claude",
		"CLAUDE_CODE_OAUTH_TOKEN=claude-oauth-secret",
		"CLAUDE_CODE_SUBAGENT_MODEL=sonnet",
		"CLAUDE_CODE_EFFORT_LEVEL=high",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"GC_HOME=/srv/controller/gc-home",
		"XDG_RUNTIME_DIR=/run/user/1001",
		"GC_COMPATIBILITY_AUTHORITY_DIR=/etc/gc/compatibility-authority",
		"GC_COMPATIBILITY_AUTHORITY_MAX_REVOCATION_AGE=15m",
		"GC_PR_HUMAN_TRUST=trusted-reviewer",
		"ANTHROPIC_API_KEY=provider-secret",
		"GC_DOLT_HOST=dolt.example.internal",
		"GC_DOLT_LOGLEVEL=warn",
		"T3_WS_URL=ws://127.0.0.1:4100/ws",
		"TZ=America/New_York",
		"LANG=C.UTF-8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("explicit controller environment is missing %q: %q", want, joined)
		}
	}
}

func TestControllerEnvironmentFileRejectsFixedKeyOverrides(t *testing.T) {
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", authorityDirectoryEnv} {
		t.Run(key, func(t *testing.T) {
			data := []byte(`{"` + key + `":"/tmp/override"}`)
			if _, err := parseControllerEnvironment(data); err == nil {
				t.Fatalf("fixed controller key %q was accepted", key)
			}
		})
	}
}

func TestControllerEnvironmentFileRejectsMalformedOrUnsafeEntries(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "malformed JSON", data: `{"LANG":`},
		{name: "top-level null", data: `null`},
		{name: "top-level array", data: `[]`},
		{name: "duplicate key", data: `{"LANG":"C","LANG":"C.UTF-8"}`},
		{name: "invalid identifier", data: `{"bad-name":"value"}`},
		{name: "non-string value", data: `{"LANG":7}`},
		{name: "empty value", data: `{"LANG":""}`},
		{name: "NUL value", data: `{"LANG":"x\u0000y"}`},
		{name: "unrecognized name", data: `{"NOT_A_CONTROLLER_INPUT":"value"}`},
		{name: "LD_PRELOAD", data: `{"LD_PRELOAD":"/tmp/inject.so"}`},
		{name: "LD_LIBRARY_PATH", data: `{"LD_LIBRARY_PATH":"/tmp/lib"}`},
		{name: "DYLD injection", data: `{"DYLD_INSERT_LIBRARIES":"/tmp/inject.dylib"}`},
		{name: "GODEBUG", data: `{"GODEBUG":"cgocheck=0"}`},
		{name: "GOTRACEBACK", data: `{"GOTRACEBACK":"crash"}`},
		{name: "GOENV", data: `{"GOENV":"/tmp/go.env"}`},
		{name: "GORACE", data: `{"GORACE":"halt_on_error=1"}`},
		{name: "relative GC_HOME", data: `{"GC_HOME":"relative/home"}`},
		{name: "relative CLAUDE_CONFIG_DIR", data: `{"CLAUDE_CONFIG_DIR":"relative/claude"}`},
		{name: "unclean XDG_RUNTIME_DIR", data: `{"XDG_RUNTIME_DIR":"/run/../tmp"}`},
		{name: "protected key material", data: `{"GC_BEADS_PROTECTED_KEY_TOKEN":"secret"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseControllerEnvironment([]byte(test.data)); err == nil {
				t.Fatalf("unsafe controller environment %s was accepted", test.name)
			}
		})
	}
}

func TestReadControllerEnvironmentFileRejectsUnsafeFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "environment.json")
	if err := os.WriteFile(path, []byte(`{"LANG":"C"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readControllerEnvironmentFile(path); err == nil {
		t.Fatal("environment file with unsafe ancestry or permissions was accepted")
	}
	if _, err := readControllerEnvironmentFile(directory + string(filepath.Separator) + "../environment.json"); err == nil {
		t.Fatal("non-clean environment file path was accepted")
	}
}

func TestExitedPidfdCannotAuthorizeKey(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestPidfdChildWaitsForTermination$")
	command.Env = []string{"GC_TEST_PIDFD_CHILD=1"}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	pidfd, err := unix.PidfdOpen(command.Process.Pid, 0)
	if err != nil {
		t.Skipf("pidfd_open is unavailable: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	digest := sha256.Sum256([]byte("policy"))
	broker := &Broker{
		digest: digest,
		keys:   map[string][]byte{"allowed": []byte("key")},
		record: &launchRecord{
			pid: command.Process.Pid, uid: uint32(os.Getuid()), pidfd: pidfd, executable: "/usr/bin/gc", arguments: []string{"/usr/bin/gc", "supervisor", "run"}, digest: digest,
			handles: map[string]struct{}{"allowed": {}},
		},
	}
	t.Cleanup(func() { _ = unix.Close(pidfd) })
	if fd, err := broker.authorizedKeyFD(command.Process.Pid, uint32(os.Getuid()), digest, "allowed"); err == nil {
		_ = unix.Close(fd)
		t.Fatal("exited pidfd authorized a key request")
	}
}

func TestBrokerRejectsUnregisteredKernelPeer(t *testing.T) {
	directory := testutil.ShortTempDir(t, "gc-broker-")
	socketPath := directory + "/broker.sock"
	listener, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(listener, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	if err := unix.Listen(listener, 8); err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("policy"))
	executable, arguments := currentProcessCommand(t)
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		_ = unix.Close(listener)
		t.Skipf("pidfd_open is unavailable: %v", err)
	}
	broker := &Broker{
		listener: listener, socketPath: socketPath, socketInfo: info, digest: digest,
		keys: map[string][]byte{"allowed": []byte("key")}, active: make(map[int]struct{}),
		connections: make(chan struct{}, maxConnections),
		record: &launchRecord{
			pid: os.Getpid(), uid: uint32(os.Getuid()), pidfd: pidfd, executable: executable, arguments: arguments, digest: digest,
			handles: map[string]struct{}{"allowed": {}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- broker.Serve(ctx) }()
	command := exec.Command(os.Args[0], "-test.run=^TestBrokerUnregisteredPeerHelper$")
	command.Env = []string{
		"GC_TEST_BROKER_SOCKET=" + socketPath,
		"GC_TEST_BROKER_DIGEST=" + hex.EncodeToString(digest[:]),
	}
	if output, err := command.CombinedOutput(); err != nil {
		cancel()
		_ = broker.Close()
		<-serveDone
		t.Fatalf("unregistered child peer failed: %v: %s", err, output)
	}
	cancel()
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func currentProcessCommand(t *testing.T) (string, []string) {
	t.Helper()
	executable, err := os.Readlink("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	commandLine, err := os.ReadFile("/proc/self/cmdline")
	if err != nil || len(commandLine) == 0 || commandLine[len(commandLine)-1] != 0 {
		t.Fatalf("read current process command line: %v", err)
	}
	argumentBytes := bytes.Split(commandLine[:len(commandLine)-1], []byte{0})
	arguments := make([]string, len(argumentBytes))
	for i, argument := range argumentBytes {
		arguments[i] = string(argument)
	}
	if len(arguments) == 0 {
		t.Fatal("test process has no argv")
	}
	if arguments[0] != executable {
		t.Skipf("test executable argv does not use its canonical path: argv0=%q executable=%q", arguments[0], executable)
	}
	return executable, arguments
}

func TestPidfdChildWaitsForTermination(_ *testing.T) {
	if os.Getenv("GC_TEST_PIDFD_CHILD") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
		defer stop()
		<-ctx.Done()
	}
}

func TestBrokerUnregisteredPeerHelper(t *testing.T) {
	socketPath := os.Getenv("GC_TEST_BROKER_SOCKET")
	if socketPath == "" {
		return
	}
	digest := os.Getenv("GC_TEST_BROKER_DIGEST")
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(keyBrokerRequest{
		SchemaVersion: brokerSchemaV1, AuthorityDocumentDigest: digest, PrivateKeyHandle: "allowed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := unix.SendmsgN(fd, payload, nil, nil, 0); err != nil || sent != len(payload) {
		t.Fatalf("send broker request: bytes=%d err=%v", sent, err)
	}
	response := make([]byte, 512)
	control := make([]byte, unix.CmsgSpace(4))
	count, controlCount, _, _, err := unix.Recvmsg(fd, response, control, 0)
	if err != nil || count != 0 || controlCount != 0 {
		t.Fatalf("unregistered kernel peer received a response: bytes=%d control=%d err=%v", count, controlCount, err)
	}
}

func TestDecodeStrictJSONRejectsDuplicateAndUnknownFields(t *testing.T) {
	var request keyBrokerRequest
	if err := decodeStrictJSON([]byte(`{"schema_version":1,"schema_version":1}`), &request); err == nil {
		t.Fatal("duplicate JSON field was accepted")
	}
	if err := decodeStrictJSON([]byte(`{"schema_version":1,"unexpected":true}`), &request); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
}
