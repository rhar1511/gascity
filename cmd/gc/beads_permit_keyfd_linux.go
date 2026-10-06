//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const requiredHostBeadsPermitMemfdSeals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE

const (
	hostBeadsPermitKeyBrokerSchemaV1          = 1
	hostBeadsPermitBrokerPIDFDAuthorizationV1 = "root-pidfd-launch-record-v1"
)

type hostBeadsPermitKeyBrokerRequest struct {
	SchemaVersion           int    `json:"schema_version"`
	AuthorityDocumentDigest string `json:"authority_document_digest"`
	PrivateKeyHandle        string `json:"private_key_handle"`
}

type hostBeadsPermitKeyBrokerResponse struct {
	SchemaVersion       int    `json:"schema_version"`
	PrivateKeyHandle    string `json:"private_key_handle"`
	AuthorizedClientPID int    `json:"authorized_client_pid"`
	AuthorizedClientUID int    `json:"authorized_client_uid"`
	ClientAuthorization string `json:"client_authorization"`
}

// receiveHostBeadsPermitPrivateKeyFD connects only after process memory is
// non-dumpable. The root-owned local broker must match SO_PEERCRED to a
// root-created, pidfd-backed launch record; client-supplied identity or
// self-registration is not sufficient. It returns one sealed anonymous memfd,
// so no key descriptor crosses exec or exists during pre-main bootstrap.
func receiveHostBeadsPermitPrivateKeyFD(socketPath string, documentDigest [sha256.Size]byte, keyHandle string) (int, error) {
	info, err := os.Lstat(socketPath)
	// A deployment may grant a dedicated controller group write permission so
	// the non-root controller can connect. The pinned root-owned directory
	// prevents replacement; root peer credentials and the pidfd launch record
	// authorize the endpoint and exact client. World-writable sockets are never
	// accepted.
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o002 != 0 || !hostBeadsPermitBrokerOwnerIsTrusted(info) {
		return -1, errors.New("private-key broker socket is not protected")
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, errors.New("open private-key broker socket failed")
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		return -1, errors.New("connect private-key broker failed")
	}
	peer, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || peer == nil || !hostBeadsPermitBrokerUIDIsTrusted(uint64(peer.Uid)) {
		return -1, errors.New("private-key broker peer is not root")
	}
	request, err := json.Marshal(hostBeadsPermitKeyBrokerRequest{
		SchemaVersion: hostBeadsPermitKeyBrokerSchemaV1, AuthorityDocumentDigest: hex.EncodeToString(documentDigest[:]),
		PrivateKeyHandle: keyHandle,
	})
	if err != nil {
		return -1, errors.New("encode private-key broker request failed")
	}
	if sent, err := unix.SendmsgN(fd, request, nil, nil, 0); err != nil || sent != len(request) {
		return -1, errors.New("send private-key broker request failed")
	}
	payload := make([]byte, 1024)
	control := make([]byte, unix.CmsgSpace(4))
	count, controlCount, flags, _, err := unix.Recvmsg(fd, payload, control, 0)
	if err != nil || count == 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return -1, errors.New("receive private-key broker response failed")
	}
	messages, err := unix.ParseSocketControlMessage(control[:controlCount])
	if err != nil || len(messages) != 1 {
		return -1, errors.New("private-key broker descriptor response is invalid")
	}
	rights, err := unix.ParseUnixRights(&messages[0])
	if err != nil || len(rights) != 1 || rights[0] < 3 {
		for _, received := range rights {
			_ = unix.Close(received)
		}
		return -1, errors.New("private-key broker did not return exactly one descriptor")
	}
	receivedFD := rights[0]
	keepReceived := false
	defer func() {
		if !keepReceived {
			_ = unix.Close(receivedFD)
		}
	}()
	var response hostBeadsPermitKeyBrokerResponse
	if err := decodeHostBeadsPermitJSON(payload[:count], &response); err != nil ||
		response.SchemaVersion != hostBeadsPermitKeyBrokerSchemaV1 || response.PrivateKeyHandle != keyHandle ||
		response.AuthorizedClientPID != os.Getpid() || response.AuthorizedClientUID != os.Geteuid() ||
		response.ClientAuthorization != hostBeadsPermitBrokerPIDFDAuthorizationV1 {
		return -1, errors.New("private-key broker response is invalid")
	}
	unix.CloseOnExec(receivedFD)
	keepReceived = true
	return receivedFD, nil
}

// readHostBeadsPermitPrivateKeyFD accepts only an anonymous, immutable memfd.
// The authenticated broker passes it to the already non-dumpable controller,
// which consumes and closes the original. No filesystem path to the private
// key exists in the controller or session environment.
func readHostBeadsPermitPrivateKeyFD(fd int, limit int64) ([]byte, error) {
	if fd < 3 || limit <= 0 {
		return nil, errors.New("private-key descriptor is invalid")
	}
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return nil, errors.New("duplicate private-key descriptor failed")
	}
	unix.CloseOnExec(duplicate)
	file := os.NewFile(uintptr(duplicate), "gascity-protected-mutation-key")
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, errors.New("open private-key descriptor failed")
	}
	defer file.Close() //nolint:errcheck

	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredHostBeadsPermitMemfdSeals != requiredHostBeadsPermitMemfdSeals {
		return nil, errors.New("private-key descriptor is not a sealed memfd")
	}
	info, err := file.Stat()
	if err != nil || !hostBeadsPermitMemfdInfoIsSafe(info, limit) {
		return nil, errors.New("private-key descriptor is not an anonymous bounded regular file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("rewind private-key descriptor failed")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit || int64(len(data)) != info.Size() {
		zeroHostBeadsPermitBytes(data)
		return nil, errors.New("private-key descriptor read failed or exceeded its size limit")
	}
	after, err := file.Stat()
	if err != nil || !hostBeadsPermitMemfdInfoIsSafe(after, limit) || after.Size() != info.Size() {
		zeroHostBeadsPermitBytes(data)
		return nil, errors.New("private-key descriptor changed while reading")
	}
	return data, nil
}

func hostBeadsPermitMemfdInfoIsSafe(info os.FileInfo, limit int64) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 0
}

func closeHostBeadsPermitPrivateKeyFD(fd int) error {
	return unix.Close(fd)
}

func lockDownHostBeadsPermitProcessMemory() error {
	// Key descriptors are requested from the local broker only after this
	// succeeds. A non-root controller without ptrace authority cannot inspect
	// workers, while same-UID workers cannot inspect this non-dumpable process.
	if os.Geteuid() == 0 {
		return errors.New("configured signing authority requires a non-root controller process")
	}
	var capabilityHeader unix.CapUserHeader
	capabilityHeader.Version = unix.LINUX_CAPABILITY_VERSION_3
	var capabilities [2]unix.CapUserData
	if err := unix.Capget(&capabilityHeader, &capabilities[0]); err != nil {
		return errors.New("read effective process capabilities failed")
	}
	if hostBeadsPermitEffectiveCapability(capabilities[:], unix.CAP_SYS_PTRACE) {
		return errors.New("configured signing authority forbids effective CAP_SYS_PTRACE")
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return errors.New("disable same-UID process inspection failed")
	}
	return nil
}

func hostBeadsPermitEffectiveCapability(capabilities []unix.CapUserData, capability int) bool {
	if capability < 0 {
		return false
	}
	word := capability / 32
	if word >= len(capabilities) {
		return false
	}
	return capabilities[word].Effective&(uint32(1)<<uint(capability%32)) != 0
}
