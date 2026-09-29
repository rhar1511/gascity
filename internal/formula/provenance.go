package formula

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// CompileProvenanceSchemaVersion versions CompileProvenance's source contract.
// Increment it if fields change meaning or new compiler inputs are added.
const CompileProvenanceSchemaVersion = 1

// CompileSourceMode describes the configured Source implementation.
type CompileSourceMode string

// CompileSourceReadMode describes the source used for one successful read.
type CompileSourceReadMode string

// CompileSourceReadKind classifies formula and external-reference reads.
type CompileSourceReadKind string

// CompileProvenanceStatus states whether captured evidence is complete enough
// for admission policy use.
type CompileProvenanceStatus string

// CompileProvenanceReason identifies a fail-closed provenance gap.
type CompileProvenanceReason string

// CompileTraceKind identifies a compiler transformation or inheritance edge.
type CompileTraceKind string

const (
	CompileSourceModeFilesystem                          CompileSourceMode       = "filesystem"
	CompileSourceModeGitRef                              CompileSourceMode       = "git-ref"
	CompileSourceModeGitRefFallback                      CompileSourceMode       = "git-ref-with-filesystem-fallback"
	CompileSourceModeUnknown                             CompileSourceMode       = "unknown"
	CompileSourceReadModeFilesystem                      CompileSourceReadMode   = "filesystem"
	CompileSourceReadModeGitRef                          CompileSourceReadMode   = "git-ref"
	CompileSourceReadModeUnknown                         CompileSourceReadMode   = "unknown"
	CompileProvenanceAvailable                           CompileProvenanceStatus = "available"
	CompileProvenanceUnavailable                         CompileProvenanceStatus = "unavailable"
	CompileProvenanceReasonCheckPathManifestMissing      CompileProvenanceReason = "check_path_dependency_manifest_missing"
	CompileProvenanceReasonAmbiguousFormulaName          CompileProvenanceReason = "ambiguous_formula_name"
	CompileProvenanceReasonInvalidFormulaIdentity        CompileProvenanceReason = "invalid_formula_source_identity"
	CompileProvenanceReasonUnclassifiedRead              CompileProvenanceReason = "unclassified_source_read"
	CompileProvenanceReasonInconsistentRead              CompileProvenanceReason = "source_returned_different_bytes_for_path"
	CompileProvenanceReasonUnknownSourceMode             CompileProvenanceReason = "source_mode_unknown"
	CompileProvenanceReasonExternalAssetIdentityMissing  CompileProvenanceReason = "external_asset_logical_identity_missing"
	CompileProvenanceReasonExternalAssetBytesUnavailable CompileProvenanceReason = "external_asset_bytes_unavailable"
	CompileSourceReadKindFormula                         CompileSourceReadKind   = "formula"
	CompileSourceReadKindDescriptionAsset                CompileSourceReadKind   = "description_asset"
	CompileSourceReadKindUnclassified                    CompileSourceReadKind   = "unclassified"
)

const (
	CompileTraceInheritance      CompileTraceKind = "inheritance"
	CompileTraceInlineExpansion  CompileTraceKind = "inline_expansion"
	CompileTraceComposeExpand    CompileTraceKind = "compose_expand"
	CompileTraceComposeMap       CompileTraceKind = "compose_map"
	CompileTraceAspect           CompileTraceKind = "aspect"
	CompileTraceStandaloneExpand CompileTraceKind = "standalone_expansion"
)

// CompileProvenance records compiler-owned inputs and source reads for an
// opt-in formula compilation. Paths are process-local evidence only; formula
// names are the stable identities used by admission policy.
type CompileProvenance struct {
	SchemaVersion             int
	CompilerCapability        string
	FormulaV2Enabled          bool
	EffectiveCompileVariables map[string]string
	Source                    CompileSourceDescriptor
	FormulaSources            []SourceIdentity
	LoadedFormulas            []SourceIdentity
	SourceReads               []CompileSourceRead
	ExternalAssets            []ExternalAssetRead
	Trace                     []CompileTraceEntry
	CheckPaths                []CompileCheckPath
	Status                    CompileProvenanceStatus
	UnavailableReasons        []CompileProvenanceReason
}

// CompileSourceDescriptor identifies the configured Source semantics used by
// one compilation. Per-read source semantics are retained separately because
// git-ref mode can fall back to the filesystem for paths outside a repository.
type CompileSourceDescriptor struct {
	Mode CompileSourceMode
	Ref  string
}

// CompileSourceRead records every successful Source.ReadFile made during
// compilation, including formula files and external description assets.
type CompileSourceRead struct {
	Sequence    int
	Path        string
	SHA256      string
	Mode        CompileSourceReadMode
	Ref         string
	Kind        CompileSourceReadKind
	FormulaName string
}

// ExternalAssetRead records exact bytes loaded through Source.ReadFile for a
// non-formula reference such as description_file. It does not claim that an
// asset path is a stable logical ID.
type ExternalAssetRead struct {
	Sequence int
	Path     string
	SHA256   string
	Mode     CompileSourceReadMode
	Ref      string
	Kind     CompileSourceReadKind
}

// CompileTraceEntry records an inheritance edge or a formula actually used by
// a compiler composition operation, in the order the compiler applies it.
type CompileTraceEntry struct {
	Sequence           int
	Kind               CompileTraceKind
	FormulaName        string
	RelatedFormulaName string
	TargetStepID       string
	Selector           string
	EffectiveVariables map[string]string
}

// CompileCheckPath records the final materialized check path on a recipe step.
// Until the compiler supplies a complete script dependency manifest, any such
// path makes admission provenance unavailable.
type CompileCheckPath struct {
	StepID string
	Path   string
}

type compileSourceReadInfo struct {
	mode CompileSourceReadMode
	ref  string
}

type compileProvenanceRecorder struct {
	descriptor   CompileSourceDescriptor
	reads        []CompileSourceRead
	readHashes   map[string]string
	formulas     map[string]SourceIdentity
	formulaNames map[string]string
	descriptions map[string]struct{}
	trace        []CompileTraceEntry
	reasons      map[CompileProvenanceReason]struct{}
	result       CompileProvenance
}

func newCompileProvenanceRecorder(source Source) *compileProvenanceRecorder {
	return &compileProvenanceRecorder{
		descriptor:   describeCompileSource(source),
		readHashes:   make(map[string]string),
		formulas:     make(map[string]SourceIdentity),
		formulaNames: make(map[string]string),
		descriptions: make(map[string]struct{}),
		reasons:      make(map[CompileProvenanceReason]struct{}),
	}
}

func (r *compileProvenanceRecorder) recordRead(path string, data []byte, info compileSourceReadInfo) {
	digest := sha256.Sum256(data)
	digestHex := hex.EncodeToString(digest[:])
	cleanPath := filepath.Clean(path)
	key := compilePathKey(cleanPath)
	if priorHash, ok := r.readHashes[key]; ok && priorHash != digestHex {
		r.addUnavailableReason(CompileProvenanceReasonInconsistentRead)
	}
	r.readHashes[key] = digestHex
	if info.mode == CompileSourceReadModeUnknown {
		r.addUnavailableReason(CompileProvenanceReasonUnknownSourceMode)
	}
	r.reads = append(r.reads, CompileSourceRead{
		Sequence: len(r.reads),
		Path:     cleanPath,
		SHA256:   digestHex,
		Mode:     info.mode,
		Ref:      info.ref,
	})
}

func (r *compileProvenanceRecorder) recordFormula(source SourceIdentity) {
	key := compilePathKey(source.Path)
	if !validStableFormulaName(source.FormulaName) || !validSHA256(source.ContentSHA256) || key == "" {
		r.addUnavailableReason(CompileProvenanceReasonInvalidFormulaIdentity)
		return
	}
	if prior, ok := r.formulas[key]; ok && (prior.FormulaName != source.FormulaName || prior.ContentSHA256 != source.ContentSHA256) {
		r.addUnavailableReason(CompileProvenanceReasonAmbiguousFormulaName)
	}
	if _, ok := r.descriptions[key]; ok {
		r.addUnavailableReason(CompileProvenanceReasonAmbiguousFormulaName)
	}
	if priorPath, ok := r.formulaNames[source.FormulaName]; ok && priorPath != key {
		r.addUnavailableReason(CompileProvenanceReasonAmbiguousFormulaName)
	}
	r.formulas[key] = source
	r.formulaNames[source.FormulaName] = key
}

func (r *compileProvenanceRecorder) recordDescription(path string, loaded bool) {
	key := compilePathKey(path)
	if key == "" {
		r.addUnavailableReason(CompileProvenanceReasonUnclassifiedRead)
		return
	}
	if !loaded {
		r.addUnavailableReason(CompileProvenanceReasonExternalAssetBytesUnavailable)
	}
	if _, ok := r.formulas[key]; ok {
		r.addUnavailableReason(CompileProvenanceReasonAmbiguousFormulaName)
	}
	r.descriptions[key] = struct{}{}
}

func (r *compileProvenanceRecorder) recordTrace(entry CompileTraceEntry) {
	entry.Sequence = len(r.trace)
	entry.EffectiveVariables = cloneStringMap(entry.EffectiveVariables)
	r.trace = append(r.trace, entry)
}

func (r *compileProvenanceRecorder) addUnavailableReason(reason CompileProvenanceReason) {
	if reason != "" {
		r.reasons[reason] = struct{}{}
	}
}

func (r *compileProvenanceRecorder) finish(recipe *Recipe, compileVars map[string]string, formulaV2Enabled bool) CompileProvenance {
	provenance := CompileProvenance{
		SchemaVersion:             CompileProvenanceSchemaVersion,
		CompilerCapability:        activeFormulaCompilerCapability(formulaV2Enabled),
		FormulaV2Enabled:          formulaV2Enabled,
		EffectiveCompileVariables: cloneStringMap(compileVars),
		Source:                    r.descriptor,
		Trace:                     append([]CompileTraceEntry(nil), r.trace...),
		Status:                    CompileProvenanceAvailable,
	}
	if recipe != nil {
		provenance.FormulaSources = append([]SourceIdentity(nil), recipe.FormulaSources...)
	}
	for _, source := range r.formulas {
		provenance.LoadedFormulas = append(provenance.LoadedFormulas, source)
	}
	slices.SortFunc(provenance.LoadedFormulas, func(a, b SourceIdentity) int {
		if result := strings.Compare(a.FormulaName, b.FormulaName); result != 0 {
			return result
		}
		if result := strings.Compare(a.ContentSHA256, b.ContentSHA256); result != 0 {
			return result
		}
		return strings.Compare(a.Path, b.Path)
	})
	for _, read := range r.reads {
		read.Kind = CompileSourceReadKindUnclassified
		if source, ok := r.formulas[compilePathKey(read.Path)]; ok {
			read.Kind = CompileSourceReadKindFormula
			read.FormulaName = source.FormulaName
		} else if _, ok := r.descriptions[compilePathKey(read.Path)]; ok {
			read.Kind = CompileSourceReadKindDescriptionAsset
			r.addUnavailableReason(CompileProvenanceReasonExternalAssetIdentityMissing)
			provenance.ExternalAssets = append(provenance.ExternalAssets, ExternalAssetRead{
				Sequence: read.Sequence,
				Path:     read.Path,
				SHA256:   read.SHA256,
				Mode:     read.Mode,
				Ref:      read.Ref,
				Kind:     read.Kind,
			})
		} else {
			r.addUnavailableReason(CompileProvenanceReasonUnclassifiedRead)
		}
		provenance.SourceReads = append(provenance.SourceReads, read)
	}
	if recipe != nil {
		for _, step := range recipe.Steps {
			if path, exists := step.Metadata[beadmeta.CheckPathMetadataKey]; exists {
				provenance.CheckPaths = append(provenance.CheckPaths, CompileCheckPath{StepID: step.ID, Path: path})
			}
		}
	}
	if len(provenance.CheckPaths) > 0 {
		r.addUnavailableReason(CompileProvenanceReasonCheckPathManifestMissing)
	}
	for reason := range r.reasons {
		provenance.UnavailableReasons = append(provenance.UnavailableReasons, reason)
	}
	slices.Sort(provenance.UnavailableReasons)
	if len(provenance.UnavailableReasons) > 0 {
		provenance.Status = CompileProvenanceUnavailable
	}
	return provenance
}

func validStableFormulaName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\\\x00\r\n\t") || filepath.IsAbs(name) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func compilePathKey(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return map[string]string{}
	}
	clone := make(map[string]string, len(input))
	for key, value := range input {
		clone[key] = value
	}
	return clone
}

type recordingFormulaSource struct {
	Source
	recorder *compileProvenanceRecorder
}

func (s recordingFormulaSource) ReadFile(path string) ([]byte, error) {
	data, info, err := readFileWithCompileSourceInfo(s.Source, path)
	if err == nil {
		s.recorder.recordRead(path, data, info)
	}
	return data, err
}

func readFileWithCompileSourceInfo(source Source, path string) ([]byte, compileSourceReadInfo, error) {
	if reporter, ok := source.(interface {
		readFileWithCompileSourceInfo(string) ([]byte, compileSourceReadInfo, error)
	}); ok {
		return reporter.readFileWithCompileSourceInfo(path)
	}
	data, err := source.ReadFile(path)
	return data, compileSourceReadInfo{mode: CompileSourceReadModeUnknown}, err
}

func describeCompileSource(source Source) CompileSourceDescriptor {
	switch current := source.(type) {
	case FSSource:
		return CompileSourceDescriptor{Mode: CompileSourceModeFilesystem}
	case *GitRefSource:
		return CompileSourceDescriptor{Mode: CompileSourceModeGitRef, Ref: current.Ref()}
	case gitRepoAwareFallback:
		return CompileSourceDescriptor{Mode: CompileSourceModeGitRefFallback, Ref: current.git.Ref()}
	case FallbackSource:
		primary := describeCompileSource(current.Primary)
		fallback := describeCompileSource(current.Fallback)
		if primary.Mode == CompileSourceModeGitRef && fallback.Mode == CompileSourceModeFilesystem {
			return CompileSourceDescriptor{Mode: CompileSourceModeGitRefFallback, Ref: primary.Ref}
		}
		return CompileSourceDescriptor{Mode: CompileSourceMode("fallback:" + string(primary.Mode) + ":" + string(fallback.Mode)), Ref: primary.Ref}
	default:
		return CompileSourceDescriptor{Mode: CompileSourceModeUnknown}
	}
}
