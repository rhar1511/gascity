package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/spf13/cobra"
)

func newBeadsAdmissionAttachCmd(stdout, stderr io.Writer) *cobra.Command {
	var storeRef, keyFile string
	cmd := &cobra.Command{
		Use: "attach <reviewed-draft.json>", Short: "Sign a reviewed draft and atomically attach its admission receipt",
		Long: `Recompute the current source and policy, verify the reviewed draft, and
sign it with the configured primary admission authority. The key must be an
owner-only Ed25519 PKCS#8 PEM file in an owner-only directory. Unsupported file
ownership checks fail closed. No private key is printed or placed in metadata.

Attachment uses the exact source revision and durable transition receipt.
An exact retry recovers the same attachment without another source write.
This command does not launch a worker or clear any dispatch hold.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			proof, err := attachBeadsAdmission(args[0], storeRef, keyFile, stderr)
			if err == nil {
				encoder := json.NewEncoder(stdout)
				encoder.SetIndent("", "  ")
				err = encoder.Encode(proof)
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc beads admission attach: %v\n", err) //nolint:errcheck // best-effort diagnostic
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "exact local source store named in the reviewed draft")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "owner-only PKCS#8 file for the configured primary admission signer")
	return cmd
}

func attachBeadsAdmission(draftPath, storeRef, keyFile string, stderr io.Writer) (worklifecycle.AdmissionAttachmentProof, error) {
	var empty worklifecycle.AdmissionAttachmentProof
	if keyFile == "" {
		return empty, fmt.Errorf("--key-file is required")
	}
	if _, _, err := parseBeadsMetadataCASStoreRef(storeRef); err != nil {
		return empty, err
	}
	data, err := readBeadsAdmissionFile(draftPath, 2<<20, false)
	if err != nil {
		return empty, fmt.Errorf("read reviewed draft: %w", err)
	}
	var draft beadsAdmissionDraft
	if err := decodeHostBeadsPermitJSON(data, &draft); err != nil {
		return empty, fmt.Errorf("reviewed draft is not strict admission JSON: %w", err)
	}
	receipt := draft.Receipt
	if draft.SchemaVersion != 1 || draft.StoreRef != storeRef || receipt.Version != 2 || receipt.ExpectedWorkRevision <= 0 || receipt.Signature != "" || !validMetadataCASToken(receipt.WorkItemID, metadataCASMaxBeadIDBytes) {
		return empty, fmt.Errorf("draft must be unsigned schema 1 with an exact v2 source identity, store and revision")
	}
	context, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return empty, err
	}
	defer context.close()
	if receipt.Scope != worklifecycle.ScopeForStore(context.cityName, storeRef) || receipt.AdmittedBy != context.cfg.Lifecycle.AdmissionV2PrimaryAuthority {
		return empty, fmt.Errorf("draft scope or primary authority differs from current configuration")
	}
	if _, ok := context.cfg.Lifecycle.AcceptanceAuthorities[receipt.AcceptanceAuthority]; !ok {
		return empty, fmt.Errorf("draft acceptance authority is not configured")
	}
	source, err := context.snapshot(receipt.WorkItemID)
	if err != nil {
		return empty, err
	}
	if source.ID != receipt.WorkItemID || source.Revision <= 0 || source.Status != "open" {
		return empty, fmt.Errorf("source must remain open with an authoritative revision")
	}
	// A replay may have the Q43 attachment revision. The adapter proves that
	// exact immutable transition; this command never invents a later revision.
	projection, err := context.policy(source, receipt)
	if err != nil {
		return empty, err
	}
	digest, err := worklifecycle.DigestAdmissionPolicyV2(projection)
	if err != nil {
		return empty, err
	}
	currentPolicy, err := json.Marshal(projection)
	if err != nil {
		return empty, err
	}
	if receipt.RoutingPolicyDigest != digest || !sameBeadsAdmissionPolicy(draft.Policy, currentPolicy) {
		return empty, fmt.Errorf("current routing or workflow policy differs from the reviewed draft; prepare and review again")
	}
	key, err := readBeadsAdmissionSigningKey(keyFile)
	if err != nil {
		return empty, err
	}
	defer zeroHostBeadsPermitBytes(key)
	trusted, err := base64.StdEncoding.DecodeString(context.cfg.Lifecycle.AdmissionV2Authorities[receipt.AdmittedBy])
	if err != nil || !bytes.Equal(trusted, key.Public().(ed25519.PublicKey)) {
		return empty, fmt.Errorf("key does not match the configured primary admission authority")
	}
	encoded, err := worklifecycle.SignAdmissionReceiptV2(receipt, key)
	if err != nil {
		return empty, err
	}
	if leaf, ok := controllerBdWriteLeaf(context.sourceLeg.store); ok {
		// Bind only the signed admission scope. Controller lifecycle patches
		// still require their separate host-authorized permit capability.
		beads.WithBdStorePrivateEvidenceAdmissionScope(receipt.Scope)(leaf)
	}
	adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(context.sourceLeg.store)
	if err != nil {
		return empty, err
	}
	_, proof, err := adapter.Attach(encoded, context.cfg.Lifecycle, receipt.Scope)
	return proof, err
}

// Policy JSON is only review material. Compare its complete JSON value with
// the recomputed projection; never deserialize it into a storage-plan proof.
func sameBeadsAdmissionPolicy(reviewed, current []byte) bool {
	canonical := func(data []byte) ([]byte, error) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	left, leftErr := canonical(reviewed)
	right, rightErr := canonical(current)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func readBeadsAdmissionSigningKey(path string) (ed25519.PrivateKey, error) {
	data, err := readBeadsAdmissionFile(path, 16<<10, true)
	if err != nil {
		return nil, fmt.Errorf("read admission key: %w", err)
	}
	defer zeroHostBeadsPermitBytes(data)
	trimmed := bytes.TrimSpace(data)
	block, rest := pem.Decode(trimmed)
	if block == nil {
		return nil, fmt.Errorf("admission key must be one Ed25519 PKCS#8 PEM block")
	}
	defer zeroHostBeadsPermitBytes(block.Bytes)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN PRIVATE KEY-----")) || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("admission key must be one Ed25519 PKCS#8 PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("admission key is not valid PKCS#8")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("admission key must be Ed25519")
	}
	return key, nil
}

func readBeadsAdmissionFile(path string, limit int64, private bool) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil || path == "" {
		return nil, fmt.Errorf("file path is required")
	}
	parent, name := filepath.Dir(abs), filepath.Base(abs)
	parentInfo, err := os.Lstat(parent)
	safeParent := func(info os.FileInfo) bool {
		return info != nil && info.IsDir() && (!private || hostBeadsPermitOwnerIsTrusted(info) && info.Mode().Perm()&0o077 == 0)
	}
	if err != nil || !safeParent(parentInfo) {
		return nil, fmt.Errorf("file parent must be a directory; key directories require trusted ownership and owner-only permissions")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("open file directory: %w", err)
	}
	defer root.Close() //nolint:errcheck // descriptor lifetime cleanup
	openedParent, err := root.Stat(".")
	if err != nil || !safeParent(openedParent) || !os.SameFile(parentInfo, openedParent) {
		return nil, fmt.Errorf("file directory changed while opening")
	}
	safeFile := func(info os.FileInfo) bool {
		return info != nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= limit &&
			(!private || hostBeadsPermitOwnerIsTrusted(info) && info.Mode().Perm()&0o077 == 0)
	}
	before, err := root.Lstat(name)
	if err != nil || !safeFile(before) {
		return nil, fmt.Errorf("file must be a bounded regular file; keys require trusted ownership and owner-only permissions")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer file.Close() //nolint:errcheck // descriptor lifetime cleanup
	opened, err := file.Stat()
	if err != nil || !safeFile(opened) || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		zeroHostBeadsPermitBytes(data)
		return nil, fmt.Errorf("file read failed or exceeded its size limit")
	}
	after, statErr := file.Stat()
	entry, entryErr := root.Lstat(name)
	parentAfter, parentErr := os.Lstat(parent)
	if statErr != nil || entryErr != nil || parentErr != nil || !safeFile(after) || !safeFile(entry) || !safeParent(parentAfter) ||
		!os.SameFile(opened, after) || !os.SameFile(opened, entry) || !os.SameFile(openedParent, parentAfter) ||
		opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		zeroHostBeadsPermitBytes(data)
		return nil, fmt.Errorf("file or directory changed while reading")
	}
	return data, nil
}
