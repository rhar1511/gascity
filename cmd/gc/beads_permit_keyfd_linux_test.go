//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostBeadsPermitEffectiveCapability(t *testing.T) {
	var capabilities [2]unix.CapUserData
	if hostBeadsPermitEffectiveCapability(capabilities[:], unix.CAP_SYS_PTRACE) {
		t.Fatal("unset CAP_SYS_PTRACE was reported effective")
	}
	capabilities[unix.CAP_SYS_PTRACE/32].Effective = uint32(1) << uint(unix.CAP_SYS_PTRACE%32)
	if !hostBeadsPermitEffectiveCapability(capabilities[:], unix.CAP_SYS_PTRACE) {
		t.Fatal("set CAP_SYS_PTRACE was not reported effective")
	}
	if hostBeadsPermitEffectiveCapability(capabilities[:1], 32) {
		t.Fatal("out-of-range capability was reported effective")
	}
	if hostBeadsPermitEffectiveCapability(capabilities[:], -1) {
		t.Fatal("negative capability was reported effective")
	}
}

func replaceHostBeadsPermitTestKey(t *testing.T, entry *hostBeadsPermitAuthorityEntry, key []byte, sealed bool) {
	t.Helper()
	fd, err := unix.MemfdCreate("gascity-test-protected-key", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	var identity unix.Stat_t
	if err := unix.Fstat(fd, &identity); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var current unix.Stat_t
		if unix.Fstat(fd, &current) == nil && current.Dev == identity.Dev && current.Ino == identity.Ino {
			_ = unix.Close(fd)
		}
	})
	if _, err := unix.Write(fd, key); err != nil {
		t.Fatal(err)
	}
	if sealed {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, requiredHostBeadsPermitMemfdSeals); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		t.Fatal(err)
	}
	entry.privateKeyFD = fd
}

func startHostBeadsPermitTestBroker(t *testing.T, socketPath string, document []byte, entries []hostBeadsPermitAuthorityEntry) {
	t.Helper()
	listener, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(listener, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	// Exercise the production controller-group access mode. The broker socket
	// may be group writable inside its protected root-owned parent.
	if err := os.Chmod(socketPath, 0o770); err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	if err := unix.Listen(listener, 16); err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	if err := unix.SetNonblock(listener, true); err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	stopReader, stopWriter, err := os.Pipe()
	if err != nil {
		_ = unix.Close(listener)
		t.Fatal(err)
	}
	digest := sha256.Sum256(document)
	keys := make(map[string]int, len(entries))
	for _, entry := range entries {
		keys[entry.PrivateKeyHandle] = entry.privateKeyFD
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		// Closing the pipe wakes Poll immediately, without elapsed-time polling
		// or closing a descriptor underneath an active accept operation.
		_ = stopWriter.Close()
		<-done
		_ = stopReader.Close()
		_ = unix.Close(listener)
		_ = os.Remove(socketPath)
	})
	go func() {
		defer close(done)
		poll := []unix.PollFd{
			{Fd: int32(listener), Events: unix.POLLIN},
			{Fd: int32(stopReader.Fd()), Events: unix.POLLIN},
		}
		for {
			if _, err := unix.Poll(poll, -1); err != nil {
				if err == unix.EINTR {
					continue
				}
				return
			}
			if poll[1].Revents != 0 {
				return
			}
			connection, _, acceptErr := unix.Accept4(listener, unix.SOCK_CLOEXEC)
			if acceptErr != nil {
				if acceptErr == unix.EAGAIN || acceptErr == unix.EWOULDBLOCK || acceptErr == unix.EINTR {
					continue
				}
				return
			}
			func() {
				defer unix.Close(connection) //nolint:errcheck
				peer, peerErr := unix.GetsockoptUcred(connection, unix.SOL_SOCKET, unix.SO_PEERCRED)
				if peerErr != nil || peer == nil {
					return
				}
				payload := make([]byte, 1024)
				count, _, _, _, readErr := unix.Recvmsg(connection, payload, nil, 0)
				if readErr != nil {
					return
				}
				var request hostBeadsPermitKeyBrokerRequest
				if json.Unmarshal(payload[:count], &request) != nil || request.SchemaVersion != hostBeadsPermitKeyBrokerSchemaV1 ||
					request.AuthorityDocumentDigest != hex.EncodeToString(digest[:]) {
					return
				}
				keyFD, ok := keys[request.PrivateKeyHandle]
				if !ok {
					return
				}
				// A production broker registers the launched controller by pidfd and
				// rejects every other peer before releasing a key. This test broker
				// serves only its test process and echoes the kernel peer identity.
				response, _ := json.Marshal(hostBeadsPermitKeyBrokerResponse{
					SchemaVersion: hostBeadsPermitKeyBrokerSchemaV1, PrivateKeyHandle: request.PrivateKeyHandle,
					AuthorizedClientPID: int(peer.Pid), AuthorizedClientUID: int(peer.Uid),
					ClientAuthorization: hostBeadsPermitBrokerPIDFDAuthorizationV1,
				})
				_, _ = unix.SendmsgN(connection, response, unix.UnixRights(keyFD), nil, 0)
			}()
		}
	}()
}
