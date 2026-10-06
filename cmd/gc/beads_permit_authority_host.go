package main

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// hostBeadsPermitAuthorityDirEnv selects the host-owned authority mount.
	// Only trusted supervisor or standalone startup code may call the loader.
	hostBeadsPermitAuthorityDirEnv = "GC_BEADS_PROTECTED_MUTATION_AUTHORITY_DIR"
	hostBeadsPermitAuthorityFile   = "beads-protected-mutation-authorities.json"

	hostBeadsPermitAuthoritySchemaV1 = 1
	maxHostBeadsPermitAuthorityBytes = 1 << 20
	maxHostBeadsPermitKeyBytes       = 16 << 10
	maxHostBeadsPermitEntries        = 256
	maxHostBeadsPermitLifetime       = 5 * time.Minute
)

type hostBeadsPermitAuthorityDocument struct {
	SchemaVersion   int                             `json:"schema_version"`
	KeyBrokerSocket string                          `json:"key_broker_socket"`
	Entries         []hostBeadsPermitAuthorityEntry `json:"entries"`
}

// hostBeadsPermitAuthorityEntry is a host-authored policy binding. The city
// and store reference are exact lookup keys; none of these values are read
// from a city pack or a request.
type hostBeadsPermitAuthorityEntry struct {
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
	privateKeyFD          int
}

type hostBeadsPermitScope struct {
	cityName string
	storeRef string
}

// hostBeadsPermitPolicy is a copy of trusted host-authored policy for one
// exact city/store pair. Callers cannot mutate the resolver's stored policy.
type hostBeadsPermitPolicy struct {
	CityName        string
	StoreRef        string
	Audience        string
	ProjectID       string
	Database        string
	KeyID           string
	Issuer          string
	Actor           string
	ProtectionClass string
	Lifetime        time.Duration
}

type hostBeadsPermitBinding struct {
	issuer *beads.ControllerBeadsPermitIssuer
	signer *hostBeadsPermitSigner
	policy hostBeadsPermitPolicy
}

// hostBeadsPermitResolver is an immutable snapshot of a protected host
// authority directory. Its only lookup key is the exact city name and
// canonical store reference supplied by trusted controller code.
type hostBeadsPermitResolver struct {
	source   *hostBeadsPermitAuthoritySource
	bindings map[hostBeadsPermitScope]hostBeadsPermitBinding
}

// resolve returns authority and policy only for an exact configured scope.
// It does not normalize caller input or consult city, pack, provider, or
// request configuration. A replaced authority directory makes the snapshot
// unavailable until startup reloads it.
func (r *hostBeadsPermitResolver) resolve(cityName, storeRef string) (*beads.ControllerBeadsPermitIssuer, hostBeadsPermitPolicy, bool) {
	if r == nil || r.source == nil || r.source.verifyRootIdentity() != nil {
		return nil, hostBeadsPermitPolicy{}, false
	}
	binding, ok := r.bindings[hostBeadsPermitScope{cityName: cityName, storeRef: storeRef}]
	if !ok || binding.issuer == nil {
		return nil, hostBeadsPermitPolicy{}, false
	}
	return binding.issuer, binding.policy, true
}

// close releases the pinned host directory. The startup owner should call it
// during shutdown after no further resolver lookups can occur.
func (r *hostBeadsPermitResolver) close() error {
	if r == nil {
		return nil
	}
	for _, binding := range r.bindings {
		if binding.signer != nil {
			binding.signer.destroy()
		}
	}
	if r.source == nil {
		return nil
	}
	return r.source.close()
}

// loadHostBeadsPermitResolverFromEnv returns nil, nil when the dedicated host
// directory variable is absent or empty. Session environments pin
// controller-only variables empty to override inherited values. Any nonempty
// configured value is authoritative and invalid input fails startup.
func loadHostBeadsPermitResolverFromEnv() (*hostBeadsPermitResolver, error) {
	directory := os.Getenv(hostBeadsPermitAuthorityDirEnv)
	if directory == "" {
		return nil, nil
	}
	if err := hostBeadsPermitLockProcessMemory(); err != nil {
		return nil, fmt.Errorf("protect configured Beads signing authority process: %w", err)
	}
	return loadHostBeadsPermitResolver(directory)
}

func loadHostBeadsPermitResolver(directory string) (*hostBeadsPermitResolver, error) {
	source, err := newHostBeadsPermitAuthoritySource(directory)
	if err != nil {
		return nil, fmt.Errorf("open configured Beads protected-mutation authority: %w", err)
	}
	resolver, err := source.load()
	if err != nil {
		closeErr := source.close()
		return nil, errors.Join(err, closeErr)
	}
	return resolver, nil
}

type hostBeadsPermitAuthoritySource struct {
	directory      string
	root           *os.Root
	rootInfo       os.FileInfo
	documentDigest [sha256.Size]byte
	documentPinned bool
}

func newHostBeadsPermitAuthoritySource(directory string) (*hostBeadsPermitAuthoritySource, error) {
	if directory == "" || strings.TrimSpace(directory) != directory || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("authority directory must be a clean absolute path")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errors.New("authority directory path must not contain symlinks")
	}
	pathInfo, err := os.Lstat(directory)
	if err != nil || !hostBeadsPermitDirectoryIsSafe(pathInfo) {
		return nil, errors.New("authority directory must be a protected real directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("open authority directory failed")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !hostBeadsPermitDirectoryIsSafe(rootInfo) || !os.SameFile(pathInfo, rootInfo) {
		closeErr := root.Close()
		return nil, errors.Join(errors.New("authority directory changed while opening"), closeErr)
	}
	return &hostBeadsPermitAuthoritySource{directory: directory, root: root, rootInfo: rootInfo}, nil
}

func (s *hostBeadsPermitAuthoritySource) load() (*hostBeadsPermitResolver, error) {
	if s == nil || s.root == nil || s.rootInfo == nil {
		return nil, errors.New("authority directory is unavailable")
	}
	if err := s.verifyRootIdentity(); err != nil {
		return nil, err
	}
	data, err := readHostBeadsPermitFile(s.root, hostBeadsPermitAuthorityFile, maxHostBeadsPermitAuthorityBytes, false)
	if err != nil {
		return nil, errors.New("read authority document failed")
	}
	defer zeroHostBeadsPermitBytes(data)
	var document hostBeadsPermitAuthorityDocument
	if err := decodeHostBeadsPermitJSON(data, &document); err != nil {
		return nil, errors.New("authority document is not strict schema-1 JSON")
	}
	if document.SchemaVersion != hostBeadsPermitAuthoritySchemaV1 || len(document.Entries) == 0 || len(document.Entries) > maxHostBeadsPermitEntries {
		return nil, errors.New("authority document schema or entry count is invalid")
	}
	if err := s.verifyRootIdentity(); err != nil {
		return nil, err
	}

	bindings := make(map[hostBeadsPermitScope]hostBeadsPermitBinding, len(document.Entries))
	keyIDs := make(map[string]struct{}, len(document.Entries))
	if !validHostBeadsPermitSocketPath(document.KeyBrokerSocket) {
		return nil, errors.New("authority document key broker socket is invalid")
	}
	documentDigest := sha256.Sum256(data)
	publicKeys := make(map[[sha256.Size]byte]struct{}, len(document.Entries))
	keyHandles := make(map[string]struct{}, len(document.Entries))
	var signers []*hostBeadsPermitSigner
	loaded := false
	defer func() {
		if !loaded {
			for _, signer := range signers {
				signer.destroy()
			}
		}
	}()
	for index, entry := range document.Entries {
		if err := validateHostBeadsPermitEntry(entry); err != nil {
			return nil, fmt.Errorf("authority entry %d is invalid: %w", index, err)
		}
		scope := hostBeadsPermitScope{cityName: entry.CityName, storeRef: entry.StoreRef}
		if _, exists := bindings[scope]; exists {
			return nil, fmt.Errorf("authority entry %d duplicates a city/store scope", index)
		}
		if _, exists := keyIDs[entry.KeyID]; exists {
			return nil, fmt.Errorf("authority entry %d duplicates a key identifier", index)
		}
		if _, exists := keyHandles[entry.PrivateKeyHandle]; exists {
			return nil, fmt.Errorf("authority entry %d duplicates a private-key handle", index)
		}
		keyFD, receiveErr := receiveHostBeadsPermitPrivateKeyFD(
			document.KeyBrokerSocket, documentDigest, entry.PrivateKeyHandle,
		)
		if receiveErr != nil {
			return nil, fmt.Errorf("authority entry %d private key broker request failed", index)
		}
		keyData, readErr := readHostBeadsPermitPrivateKeyFD(keyFD, maxHostBeadsPermitKeyBytes)
		closeErr := closeHostBeadsPermitPrivateKeyFD(keyFD)
		if readErr != nil {
			return nil, fmt.Errorf("authority entry %d private key descriptor is unavailable or unsafe", index)
		}
		if closeErr != nil {
			zeroHostBeadsPermitBytes(keyData)
			return nil, fmt.Errorf("authority entry %d private key descriptor close failed", index)
		}
		signer, publicKey, parseErr := parseHostBeadsPermitPrivateKey(keyData)
		zeroHostBeadsPermitBytes(keyData)
		if parseErr != nil {
			return nil, fmt.Errorf("authority entry %d private key is invalid", index)
		}
		signer.authorityAvailable = s.verifyAuthorityAvailable
		signers = append(signers, signer)
		fingerprint := sha256.Sum256(publicKey)
		if _, exists := publicKeys[fingerprint]; exists {
			return nil, fmt.Errorf("authority entry %d duplicates a signing key", index)
		}
		issuer, issuerErr := beads.NewControllerBeadsPermitIssuer(beads.ControllerBeadsPermitIssuerConfig{
			Audience: entry.Audience, ProjectID: entry.ProjectID, Database: entry.Database,
			KeyID: entry.KeyID, Issuer: entry.Issuer,
			Lifetime: time.Duration(entry.PermitLifetimeSeconds) * time.Second, Signer: signer,
		})
		if issuerErr != nil {
			return nil, fmt.Errorf("authority entry %d signing authority is invalid", index)
		}
		policy := hostBeadsPermitPolicy{
			CityName: entry.CityName, StoreRef: entry.StoreRef,
			Audience: entry.Audience, ProjectID: entry.ProjectID, Database: entry.Database,
			KeyID: entry.KeyID, Issuer: entry.Issuer, Actor: entry.Actor,
			ProtectionClass: entry.ProtectionClass,
			Lifetime:        time.Duration(entry.PermitLifetimeSeconds) * time.Second,
		}
		bindings[scope] = hostBeadsPermitBinding{issuer: issuer, signer: signer, policy: policy}
		keyIDs[entry.KeyID] = struct{}{}
		keyHandles[entry.PrivateKeyHandle] = struct{}{}
		publicKeys[fingerprint] = struct{}{}
	}
	if err := s.verifyRootIdentity(); err != nil {
		return nil, err
	}
	s.documentDigest = documentDigest
	s.documentPinned = true
	if err := s.verifyAuthorityAvailable(); err != nil {
		return nil, errors.New("authority document changed while loading")
	}
	loaded = true
	return &hostBeadsPermitResolver{source: s, bindings: bindings}, nil
}

func (s *hostBeadsPermitAuthoritySource) verifyAuthorityAvailable() error {
	if err := s.verifyRootIdentity(); err != nil {
		return err
	}
	if !s.documentPinned {
		return errors.New("authority document is not pinned")
	}
	data, err := readHostBeadsPermitFile(s.root, hostBeadsPermitAuthorityFile, maxHostBeadsPermitAuthorityBytes, false)
	if err != nil {
		return errors.New("authority document is unavailable")
	}
	defer zeroHostBeadsPermitBytes(data)
	if sha256.Sum256(data) != s.documentDigest {
		return errors.New("authority document changed")
	}
	return s.verifyRootIdentity()
}

func (s *hostBeadsPermitAuthoritySource) verifyRootIdentity() error {
	if s == nil || s.root == nil || s.rootInfo == nil {
		return errors.New("authority directory is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(s.directory)
	if err != nil || resolved != s.directory {
		return errors.New("authority directory path changed")
	}
	pathInfo, err := os.Lstat(s.directory)
	if err != nil || !hostBeadsPermitDirectoryIsSafe(pathInfo) || !os.SameFile(s.rootInfo, pathInfo) {
		return errors.New("authority directory path changed")
	}
	openedInfo, err := s.root.Stat(".")
	if err != nil || !hostBeadsPermitDirectoryIsSafe(openedInfo) || !os.SameFile(s.rootInfo, openedInfo) {
		return errors.New("authority directory handle changed")
	}
	return nil
}

func (s *hostBeadsPermitAuthoritySource) close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

func validateHostBeadsPermitEntry(entry hostBeadsPermitAuthorityEntry) error {
	if !validHostBeadsPermitName(entry.CityName, 255) || strings.ContainsAny(entry.CityName, " \t\r\n:") {
		return errors.New("city name is invalid")
	}
	if !validHostBeadsPermitStoreRef(entry.StoreRef, entry.CityName) {
		return errors.New("store reference is not canonical")
	}
	if !validHostBeadsPermitName(entry.Actor, 255) || !validHostBeadsPermitName(entry.ProtectionClass, 255) {
		return errors.New("actor or protection class is invalid")
	}
	if !validHostBeadsPermitName(entry.PrivateKeyHandle, 255) || strings.ContainsAny(entry.PrivateKeyHandle, " /\\\t\r\n") {
		return errors.New("private-key handle is invalid")
	}
	if entry.PermitLifetimeSeconds < 1 || entry.PermitLifetimeSeconds > int64(maxHostBeadsPermitLifetime/time.Second) {
		return errors.New("permit lifetime is outside the supported range")
	}
	return nil
}

func validHostBeadsPermitSocketPath(value string) bool {
	return validHostBeadsPermitName(value, 107) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validHostBeadsPermitName(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validHostBeadsPermitStoreRef(value, cityName string) bool {
	if !validHostBeadsPermitName(value, 512) {
		return false
	}
	kind, name, found := strings.Cut(value, ":")
	if !found || (kind != "city" && kind != "rig") || name == "" || strings.Contains(name, ":") || strings.ContainsAny(name, " \t\r\n") {
		return false
	}
	return kind != "city" || name == cityName
}

func validHostBeadsPermitFilename(value string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.VolumeName(value) != "" || filepath.Clean(value) != value || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, "/\\\x00")
}

func parseHostBeadsPermitPrivateKey(data []byte) (*hostBeadsPermitSigner, ed25519.PublicKey, error) {
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN PRIVATE KEY-----")) {
		return nil, nil, errors.New("private key must be one PKCS#8 PEM block")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil {
		return nil, nil, errors.New("private key must be one PKCS#8 PEM block")
	}
	defer zeroHostBeadsPermitBytes(block.Bytes)
	if block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("private key must be one PKCS#8 PEM block")
	}
	var envelope struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}
	trailing, err := asn1.Unmarshal(block.Bytes, &envelope)
	if err != nil || len(trailing) != 0 {
		zeroHostBeadsPermitBytes(envelope.PrivateKey)
		return nil, nil, errors.New("private key is not valid PKCS#8")
	}
	defer zeroHostBeadsPermitBytes(envelope.PrivateKey)
	if envelope.Version != 0 || !envelope.Algorithm.Algorithm.Equal(ed25519PrivateKeyOID) {
		return nil, nil, errors.New("private key is not Ed25519")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, errors.New("private key is not valid PKCS#8")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		if ok {
			zeroHostBeadsPermitBytes(privateKey)
		}
		return nil, nil, errors.New("private key is not Ed25519")
	}
	defer zeroHostBeadsPermitBytes(privateKey)
	ownedKey := append(ed25519.PrivateKey(nil), privateKey...)
	publicKey, ok := ownedKey.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		zeroHostBeadsPermitBytes(ownedKey)
		return nil, nil, errors.New("private key public type is not Ed25519")
	}
	return &hostBeadsPermitSigner{privateKey: ownedKey, publicKey: append(ed25519.PublicKey(nil), publicKey...)}, append(ed25519.PublicKey(nil), publicKey...), nil
}

var ed25519PrivateKeyOID = asn1.ObjectIdentifier{1, 3, 101, 112}

// hostBeadsPermitSigner owns the only retained private-key copy. destroy
// prevents later signing and overwrites that copy after resolver shutdown.
type hostBeadsPermitSigner struct {
	mu                 sync.RWMutex
	privateKey         ed25519.PrivateKey
	publicKey          ed25519.PublicKey
	authorityAvailable func() error
}

func (s *hostBeadsPermitSigner) Public() crypto.PublicKey {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append(ed25519.PublicKey(nil), s.publicKey...)
}

func (s *hostBeadsPermitSigner) Sign(_ io.Reader, message []byte, opts crypto.SignerOpts) ([]byte, error) {
	if s == nil || opts == nil || opts.HashFunc() != crypto.Hash(0) {
		return nil, errors.New("host Beads permit signer is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("host Beads permit signer is destroyed")
	}
	if s.authorityAvailable == nil || s.authorityAvailable() != nil {
		return nil, errors.New("host Beads permit signing authority is unavailable")
	}
	signature := ed25519.Sign(s.privateKey, message)
	// Detect replacement that raced the signature operation. The signature is
	// discarded unless the pinned authority directory is still authoritative.
	if s.authorityAvailable() != nil {
		zeroHostBeadsPermitBytes(signature)
		return nil, errors.New("host Beads permit signing authority is unavailable")
	}
	return signature, nil
}

func (s *hostBeadsPermitSigner) destroy() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	zeroHostBeadsPermitBytes(s.privateKey)
	s.privateKey = nil
}

func decodeHostBeadsPermitJSON(data []byte, destination any) error {
	if err := rejectDuplicateHostJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateHostJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeHostJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func consumeHostJSONValue(decoder *json.Decoder) error {
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
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := consumeHostJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("JSON object is unterminated")
		}
	case '[':
		for decoder.More() {
			if err := consumeHostJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("JSON array is unterminated")
		}
	default:
		return errors.New("JSON delimiter is invalid")
	}
	return nil
}

func readHostBeadsPermitFile(root *os.Root, name string, limit int64, private bool) ([]byte, error) {
	if root == nil || !validHostBeadsPermitFilename(name) || limit <= 0 {
		return nil, errors.New("authority filename is invalid")
	}
	info, err := root.Lstat(name)
	if err != nil || !hostBeadsPermitFileIsSafe(info, limit, private) {
		return nil, errors.New("authority file is not a protected regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("open authority file failed")
	}
	openedInfo, err := file.Stat()
	if err != nil || !hostBeadsPermitFileIsSafe(openedInfo, limit, private) || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("authority file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		_ = file.Close()
		zeroHostBeadsPermitBytes(data)
		return nil, errors.New("authority file read failed or exceeded its size limit")
	}
	readInfo, err := file.Stat()
	closeErr := file.Close()
	if err != nil || closeErr != nil || !hostBeadsPermitFileIsSafe(readInfo, limit, private) ||
		!os.SameFile(openedInfo, readInfo) || openedInfo.Size() != readInfo.Size() || !openedInfo.ModTime().Equal(readInfo.ModTime()) {
		zeroHostBeadsPermitBytes(data)
		return nil, errors.New("authority file changed while reading")
	}
	return data, nil
}

func hostBeadsPermitDirectoryIsSafe(info os.FileInfo) bool {
	return info != nil && hostBeadsPermitAuthorityOwnerIsTrusted(info) && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0
}

func hostBeadsPermitFileIsSafe(info os.FileInfo, limit int64, private bool) bool {
	if info == nil || !hostBeadsPermitAuthorityOwnerIsTrusted(info) || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > limit {
		return false
	}
	return !private || info.Mode().Perm()&0o077 == 0
}

func zeroHostBeadsPermitBytes(data []byte) {
	for index := range data {
		data[index] = 0
	}
}
