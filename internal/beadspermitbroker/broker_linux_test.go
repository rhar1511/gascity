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
	"strings"
	"testing"
	"time"

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
	if validLaunchPayload(valid) {
		t.Fatal("unlisted environment variable was accepted by launch helper")
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

func TestExitedPidfdCannotAuthorizeKey(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestPidfdChildSleeps$")
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
	directory := t.TempDir()
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

func TestPidfdChildSleeps(t *testing.T) {
	if os.Getenv("GC_TEST_PIDFD_CHILD") == "1" {
		time.Sleep(10 * time.Minute)
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
