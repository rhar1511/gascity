//go:build linux

// Package beadspermitbroker implements the root-side launcher and one-child
// key broker for protected Beads signing authority.
package beadspermitbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	authorityDirectoryEnv = "GC_BEADS_PROTECTED_MUTATION_AUTHORITY_DIR"
	authorityDocumentName = "beads-protected-mutation-authorities.json"
	brokerSchemaV1        = 1
	clientAuthorizationV1 = "root-pidfd-launch-record-v1"
	maxDocumentBytes      = 1 << 20
	maxPrivateKeyBytes    = 16 << 10
	maxBrokerRequestBytes = 2048
	maxConnections        = 32
	privateKeyMemfdSeals  = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
)

type keyBrokerRequest struct {
	SchemaVersion           int    `json:"schema_version"`
	AuthorityDocumentDigest string `json:"authority_document_digest"`
	PrivateKeyHandle        string `json:"private_key_handle"`
}

type keyBrokerResponse struct {
	SchemaVersion       int    `json:"schema_version"`
	PrivateKeyHandle    string `json:"private_key_handle"`
	AuthorizedClientPID int    `json:"authorized_client_pid"`
	AuthorizedClientUID int    `json:"authorized_client_uid"`
	ClientAuthorization string `json:"client_authorization"`
}

type authorityDocument struct {
	SchemaVersion   int              `json:"schema_version"`
	KeyBrokerSocket string           `json:"key_broker_socket"`
	Entries         []authorityEntry `json:"entries"`
}

type authorityEntry struct {
	CityName              string `json:"city_name"`
	StoreRef              string `json:"store_ref"`
	Audience              string `json:"audience"`
	ProjectID             string `json:"project_id"`
	Database              string `json:"database"`
	KeyID                 string `json:"key_id"`
	Issuer                string `json:"issuer"`
	Actor                 string `json:"actor"`
	ProtectionClass       string `json:"protection_class"`
	PermitLifetimeSeconds int64  `json:"permit_lifetime_seconds"`
	PrivateKeyHandle      string `json:"private_key_handle"`
}

type launchRecord struct {
	pid        int
	uid        uint32
	pidfd      int
	executable string
	arguments  []string
	digest     [sha256.Size]byte
	handles    map[string]struct{}
}

// Broker holds one root-loaded policy and the private keys allowed by it.
// A Broker serves exactly one registered controller process.
type Broker struct {
	mu          sync.Mutex
	listener    int
	socketPath  string
	socketInfo  os.FileInfo
	digest      [sha256.Size]byte
	keys        map[string][]byte
	record      *launchRecord
	active      map[int]struct{}
	closed      bool
	handlers    sync.WaitGroup
	connections chan struct{}
}

// Config identifies the root-owned authority and key directories. Key files
// are named <private_key_handle>.pem and must be root-owned mode 0600 files.
type Config struct {
	AuthorityDirectory  string
	PrivateKeyDirectory string
	ControllerGID       uint32
}

// New loads and pins the policy and private keys, then creates the protected
// local socket. The caller must run as root and close the broker on exit.
func New(config Config) (*Broker, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("protected Beads authority launcher must run as root")
	}
	authorityRoot, err := openTrustedDirectory(config.AuthorityDirectory)
	if err != nil {
		return nil, fmt.Errorf("open protected authority directory: %w", err)
	}
	defer authorityRoot.Close() //nolint:errcheck
	keyRoot, err := openTrustedDirectory(config.PrivateKeyDirectory)
	if err != nil {
		return nil, fmt.Errorf("open protected key directory: %w", err)
	}
	defer keyRoot.Close() //nolint:errcheck

	documentBytes, err := readRootFile(authorityRoot, authorityDocumentName, maxDocumentBytes, false)
	if err != nil {
		return nil, fmt.Errorf("read protected authority policy: %w", err)
	}
	defer zero(documentBytes)
	var document authorityDocument
	if err := decodeStrictJSON(documentBytes, &document); err != nil {
		return nil, fmt.Errorf("decode protected authority policy: %w", err)
	}
	if err := validateDocument(document); err != nil {
		return nil, fmt.Errorf("validate protected authority policy: %w", err)
	}

	broker := &Broker{
		socketPath:  document.KeyBrokerSocket,
		digest:      sha256.Sum256(documentBytes),
		keys:        make(map[string][]byte, len(document.Entries)),
		active:      make(map[int]struct{}),
		connections: make(chan struct{}, maxConnections),
		listener:    -1,
	}
	for _, entry := range document.Entries {
		name := entry.PrivateKeyHandle + ".pem"
		data, readErr := readRootFile(keyRoot, name, maxPrivateKeyBytes, true)
		if readErr != nil {
			broker.destroyKeys()
			return nil, fmt.Errorf("load protected key handle %q: %w", entry.PrivateKeyHandle, readErr)
		}
		broker.keys[entry.PrivateKeyHandle] = data
	}
	if err := broker.listen(config.ControllerGID); err != nil {
		broker.destroyKeys()
		return nil, fmt.Errorf("listen on protected key broker: %w", err)
	}
	return broker, nil
}

// RegisterLaunch pins the helper PID before it execs gc. pidfd keeps the
// record attached to that exact process identity even after PID reuse.
func (b *Broker) RegisterLaunch(pid int, uid uint32, executable string, arguments []string) error {
	if b == nil || pid <= 0 || uid == 0 || !validCommand(executable, arguments) {
		return errors.New("protected controller launch identity is invalid")
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("open controller pidfd: %w", err)
	}
	if !pidfdIsLive(pidfd) {
		_ = unix.Close(pidfd)
		return errors.New("controller exited before launch registration")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.keys) == 0 || b.record != nil {
		_ = unix.Close(pidfd)
		return errors.New("protected controller launch cannot be registered")
	}
	handles := make(map[string]struct{}, len(b.keys))
	for handle := range b.keys {
		handles[handle] = struct{}{}
	}
	b.record = &launchRecord{
		pid: pid, uid: uid, pidfd: pidfd, executable: executable,
		arguments: append([]string(nil), arguments...), digest: b.digest, handles: handles,
	}
	return nil
}

// Serve accepts key requests until Close is called or ctx is cancelled.
func (b *Broker) Serve(ctx context.Context) error {
	if b == nil {
		return errors.New("protected key broker is unavailable")
	}
	b.mu.Lock()
	listener := b.listener
	b.mu.Unlock()
	if listener < 0 {
		return errors.New("protected key broker is closed")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		fds := []unix.PollFd{{Fd: int32(listener), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 200)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("poll protected broker socket: %w", err)
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if closed {
				return nil
			}
			return errors.New("protected broker socket stopped unexpectedly")
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		connection, _, err := unix.Accept4(listener, unix.SOCK_CLOEXEC)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("accept protected broker connection: %w", err)
		}
		select {
		case b.connections <- struct{}{}:
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				<-b.connections
				_ = unix.Close(connection)
				return nil
			}
			b.active[connection] = struct{}{}
			b.handlers.Add(1)
			b.mu.Unlock()
			go b.serveConnection(connection)
		default:
			_ = unix.Close(connection)
		}
	}
}

// Close stops requests, revokes the pidfd launch record, removes the socket,
// and clears all retained key bytes.
func (b *Broker) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	listener := b.listener
	b.listener = -1
	active := make([]int, 0, len(b.active))
	for fd := range b.active {
		active = append(active, fd)
	}
	record := b.record
	b.record = nil
	b.mu.Unlock()

	var closeErr error
	if listener >= 0 {
		closeErr = errors.Join(closeErr, unix.Close(listener))
	}
	for _, fd := range active {
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
	}
	b.handlers.Wait()
	if record != nil && record.pidfd >= 0 {
		closeErr = errors.Join(closeErr, unix.Close(record.pidfd))
	}
	if b.socketInfo != nil {
		if current, err := os.Lstat(b.socketPath); err == nil && os.SameFile(b.socketInfo, current) {
			closeErr = errors.Join(closeErr, os.Remove(b.socketPath))
		}
	}
	b.destroyKeys()
	return closeErr
}

func (b *Broker) listen(group uint32) error {
	if err := verifyTrustedPathDirectory(filepath.Dir(b.socketPath)); err != nil {
		return fmt.Errorf("broker socket parent is not protected: %w", err)
	}
	if _, err := os.Lstat(b.socketPath); err == nil {
		return errors.New("broker socket path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return err
	}
	bound := false
	created := false
	defer func() {
		if !bound {
			_ = unix.Close(fd)
			if created {
				_ = os.Remove(b.socketPath)
			}
		}
	}()
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: b.socketPath}); err != nil {
		return err
	}
	created = true
	if err := os.Chmod(b.socketPath, 0o660); err != nil {
		return err
	}
	if err := os.Chown(b.socketPath, 0, int(group)); err != nil {
		return err
	}
	info, err := os.Lstat(b.socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !rootOwned(info) || info.Mode().Perm() != 0o660 {
		return errors.New("broker socket identity or permissions are unsafe")
	}
	if err := unix.Listen(fd, 64); err != nil {
		return err
	}
	b.listener = fd
	b.socketInfo = info
	bound = true
	return nil
}

func (b *Broker) serveConnection(connection int) {
	defer b.handlers.Done()
	defer func() {
		_ = unix.Close(connection)
		b.mu.Lock()
		delete(b.active, connection)
		b.mu.Unlock()
		<-b.connections
	}()
	if err := unix.SetsockoptTimeval(connection, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		return
	}
	if err := unix.SetsockoptTimeval(connection, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &unix.Timeval{Sec: 2}); err != nil {
		return
	}
	peer, err := unix.GetsockoptUcred(connection, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil || peer == nil {
		return
	}
	payload := make([]byte, maxBrokerRequestBytes)
	count, _, flags, _, err := unix.Recvmsg(connection, payload, nil, 0)
	if err != nil || count == 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return
	}
	var request keyBrokerRequest
	if decodeStrictJSON(payload[:count], &request) != nil || request.SchemaVersion != brokerSchemaV1 || !validHandle(request.PrivateKeyHandle) {
		return
	}
	digestBytes, err := hex.DecodeString(request.AuthorityDocumentDigest)
	if err != nil || len(digestBytes) != sha256.Size {
		return
	}
	var digest [sha256.Size]byte
	copy(digest[:], digestBytes)
	keyFD, err := b.authorizedKeyFD(int(peer.Pid), peer.Uid, digest, request.PrivateKeyHandle)
	if err != nil {
		return
	}
	defer unix.Close(keyFD) //nolint:errcheck
	response, err := json.Marshal(keyBrokerResponse{
		SchemaVersion: brokerSchemaV1, PrivateKeyHandle: request.PrivateKeyHandle,
		AuthorizedClientPID: int(peer.Pid), AuthorizedClientUID: int(peer.Uid),
		ClientAuthorization: clientAuthorizationV1,
	})
	if err != nil {
		return
	}
	if sent, err := unix.SendmsgN(connection, response, unix.UnixRights(keyFD), nil, 0); err != nil || sent != len(response) {
		return
	}
}

func (b *Broker) authorizedKeyFD(pid int, uid uint32, digest [sha256.Size]byte, handle string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.record == nil || b.record.pid != pid || b.record.uid != uid ||
		b.record.digest != digest || digest != b.digest || !pidfdIsLive(b.record.pidfd) ||
		!processCommandMatches(pid, b.record.executable, b.record.arguments) || !pidfdIsLive(b.record.pidfd) {
		return -1, errors.New("broker peer is not the registered live controller")
	}
	if _, ok := b.record.handles[handle]; !ok {
		return -1, errors.New("key handle is outside the registered policy")
	}
	key, ok := b.keys[handle]
	if !ok {
		return -1, errors.New("key handle is unavailable")
	}
	return sealedMemfd(key)
}

func validCommand(executable string, arguments []string) bool {
	if executable == "" || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || len(arguments) == 0 || arguments[0] != executable {
		return false
	}
	for _, argument := range arguments {
		if strings.ContainsRune(argument, '\x00') {
			return false
		}
	}
	return true
}

func processCommandMatches(pid int, executable string, arguments []string) bool {
	if pid <= 0 || !validCommand(executable, arguments) {
		return false
	}
	actualExecutable, err := os.Readlink(filepath.Join("/proc", fmt.Sprint(pid), "exe"))
	if err != nil || actualExecutable != executable {
		return false
	}
	commandLine, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
	if err != nil || len(commandLine) == 0 || commandLine[len(commandLine)-1] != 0 {
		return false
	}
	actualArguments := bytes.Split(commandLine[:len(commandLine)-1], []byte{0})
	if len(actualArguments) != len(arguments) {
		return false
	}
	for i, argument := range arguments {
		if string(actualArguments[i]) != argument {
			return false
		}
	}
	return true
}

func sealedMemfd(data []byte) (int, error) {
	fd, err := unix.MemfdCreate("gc-beads-protected-key", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return -1, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	for len(data) > 0 {
		count, writeErr := unix.Write(fd, data)
		if writeErr != nil {
			return -1, writeErr
		}
		if count == 0 {
			return -1, io.ErrShortWrite
		}
		data = data[count:]
	}
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return -1, err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, privateKeyMemfdSeals); err != nil {
		return -1, err
	}
	closeOnError = false
	return fd, nil
}

func (b *Broker) destroyKeys() {
	for handle, data := range b.keys {
		zero(data)
		delete(b.keys, handle)
	}
}

func pidfdIsLive(fd int) bool {
	if fd < 0 {
		return false
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n == 0
}

func validateDocument(document authorityDocument) error {
	if document.SchemaVersion != brokerSchemaV1 || len(document.Entries) == 0 || len(document.Entries) > 256 {
		return errors.New("authority schema or entry count is invalid")
	}
	if !filepath.IsAbs(document.KeyBrokerSocket) || filepath.Clean(document.KeyBrokerSocket) != document.KeyBrokerSocket || len(document.KeyBrokerSocket) > 107 {
		return errors.New("authority broker socket path is invalid")
	}
	handles := make(map[string]struct{}, len(document.Entries))
	for _, entry := range document.Entries {
		if !validHandle(entry.PrivateKeyHandle) || strings.TrimSpace(entry.CityName) != entry.CityName || entry.CityName == "" ||
			entry.StoreRef == "" || entry.Audience == "" || entry.ProjectID == "" || entry.Database == "" || entry.KeyID == "" ||
			entry.Issuer == "" || entry.Actor == "" || entry.ProtectionClass == "" || entry.PermitLifetimeSeconds < 1 || entry.PermitLifetimeSeconds > 300 {
			return errors.New("authority entry is invalid")
		}
		if _, exists := handles[entry.PrivateKeyHandle]; exists {
			return errors.New("authority document repeats a private key handle")
		}
		handles[entry.PrivateKeyHandle] = struct{}{}
	}
	return nil
}

func validHandle(value string) bool {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, " /\\\t\r\n") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func decodeStrictJSON(data []byte, destination any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("JSON delimiter is invalid")
	}
}

func openTrustedDirectory(path string) (*os.Root, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("directory path must be a clean absolute path")
	}
	if err := verifyTrustedPathDirectory(path); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !safeRootDirectory(info) || !os.SameFile(info, pathInfo) {
		_ = root.Close()
		return nil, errors.New("directory changed while opening")
	}
	return root, nil
}

func verifyTrustedPathDirectory(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("path contains a symlink or is unavailable")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !safeRootDirectory(info) {
			return fmt.Errorf("directory %q is not a protected root-owned directory", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func safeRootDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && rootOwned(info) && info.Mode().Perm()&0o022 == 0
}

func rootOwned(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func readRootFile(root *os.Root, name string, limit int64, private bool) ([]byte, error) {
	if root == nil || name == "" || filepath.Base(name) != name || limit <= 0 {
		return nil, errors.New("file name is invalid")
	}
	info, err := root.Lstat(name)
	if err != nil || !safeRootFile(info, limit, private) {
		return nil, errors.New("file is not a protected regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !safeRootFile(openedInfo, limit, private) || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	readInfo, statErr := file.Stat()
	closeErr := file.Close()
	if err != nil || int64(len(data)) > limit || statErr != nil || closeErr != nil || !safeRootFile(readInfo, limit, private) ||
		!os.SameFile(openedInfo, readInfo) || openedInfo.Size() != readInfo.Size() || !openedInfo.ModTime().Equal(readInfo.ModTime()) {
		zero(data)
		return nil, errors.New("file changed while reading or exceeded its size limit")
	}
	return data, nil
}

func safeRootFile(info os.FileInfo, limit int64, private bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !rootOwned(info) ||
		info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > limit {
		return false
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func zero(data []byte) {
	for i := range data {
		data[i] = 0
	}
}
