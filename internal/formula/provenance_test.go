package formula

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCompileWithProvenanceCapturesSourceIdentityInputsAndInheritance(t *testing.T) {
	enableV2ForTest(t)
	t.Setenv("GC_FORMULA_REF", "")

	dir := t.TempDir()
	parent := `formula = "parent"
version = 1

[vars.mode]
default = "parent-default"

[vars.channel]
default = "stable"

[[steps]]
id = "base"
title = "Base"
`
	child := `formula = "child"
version = 1
extends = ["parent"]

[[steps]]
id = "task"
title = "Task"
`
	writeFormulaTestFile(t, filepath.Join(dir, "parent.toml"), parent)
	writeFormulaTestFile(t, filepath.Join(dir, "child.toml"), child)

	recipe, provenance, err := CompileWithProvenance(context.Background(), "child", []string{dir}, map[string]string{"mode": "requested"})
	if err != nil {
		t.Fatalf("CompileWithProvenance: %v", err)
	}
	if recipe == nil {
		t.Fatal("recipe is nil")
	}
	if provenance.SchemaVersion != CompileProvenanceSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", provenance.SchemaVersion, CompileProvenanceSchemaVersion)
	}
	if provenance.CompilerCapability != currentFormulaCompilerCapability {
		t.Fatalf("CompilerCapability = %q, want %q", provenance.CompilerCapability, currentFormulaCompilerCapability)
	}
	if !provenance.FormulaV2Enabled {
		t.Fatal("FormulaV2Enabled = false, want true")
	}
	if got := provenance.EffectiveCompileVariables["mode"]; got != "requested" {
		t.Fatalf("effective mode = %q, want requested", got)
	}
	if got := provenance.EffectiveCompileVariables["channel"]; got != "stable" {
		t.Fatalf("effective channel = %q, want inherited default stable", got)
	}
	if got := provenance.Source.Mode; got != CompileSourceModeFilesystem {
		t.Fatalf("source mode = %q, want %q", got, CompileSourceModeFilesystem)
	}
	if len(provenance.FormulaSources) != 2 {
		t.Fatalf("FormulaSources = %#v, want parent and child", provenance.FormulaSources)
	}
	if got := formulaNames(provenance.FormulaSources); !slices.Equal(got, []string{"child", "parent"}) {
		t.Fatalf("formula source names = %v, want [child parent]", got)
	}
	for _, source := range provenance.FormulaSources {
		if source.FormulaName == "" || !validSHA256(source.ContentSHA256) {
			t.Errorf("formula source lacks stable identity/hash: %#v", source)
		}
	}
	formulaReadCount := 0
	for _, read := range provenance.SourceReads {
		if read.Kind == CompileSourceReadKindFormula {
			formulaReadCount++
		}
	}
	if formulaReadCount != len(provenance.FormulaSources) {
		t.Fatalf("formula read count = %d, want %d", formulaReadCount, len(provenance.FormulaSources))
	}
	if !slices.ContainsFunc(provenance.Trace, func(entry CompileTraceEntry) bool {
		return entry.Kind == CompileTraceInheritance && entry.FormulaName == "child" && entry.RelatedFormulaName == "parent"
	}) {
		t.Fatalf("trace has no child-to-parent inheritance edge: %#v", provenance.Trace)
	}
	if provenance.Status != CompileProvenanceAvailable {
		t.Fatalf("Status = %q, want available: %v", provenance.Status, provenance.UnavailableReasons)
	}
}

func TestCompileWithProvenanceCapturesDescriptionBytesAndFailsClosedOnCheckPath(t *testing.T) {
	enableV2ForTest(t)
	t.Setenv("GC_FORMULA_REF", "")

	dir := t.TempDir()
	prompt := "Reviewed instructions from an external description asset.\n"
	writeFormulaTestFile(t, filepath.Join(dir, "prompt.md"), prompt)
	formulaText := `formula = "checked"
version = 1

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "task"
title = "Task"
description_file = "prompt.md"

[steps.check]
max_attempts = 2

[steps.check.check]
mode = "exec"
path = "scripts/check.sh"
`
	writeFormulaTestFile(t, filepath.Join(dir, "checked.toml"), formulaText)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFormulaTestFile(t, filepath.Join(dir, "scripts", "check.sh"), "#!/bin/sh\nexit 0\n")

	_, provenance, err := CompileWithProvenance(context.Background(), "checked", []string{dir}, nil)
	if err != nil {
		t.Fatalf("CompileWithProvenance: %v", err)
	}
	if provenance.Status != CompileProvenanceUnavailable {
		t.Fatalf("Status = %q, want unavailable for a check_path", provenance.Status)
	}
	if len(provenance.CheckPaths) != 1 || provenance.CheckPaths[0].StepID != "checked.task" || provenance.CheckPaths[0].Path != "scripts/check.sh" {
		t.Fatalf("CheckPaths = %#v, want the compiled check path", provenance.CheckPaths)
	}
	if !slices.Contains(provenance.UnavailableReasons, CompileProvenanceReasonCheckPathManifestMissing) {
		t.Fatalf("UnavailableReasons = %v, want check-path manifest gap", provenance.UnavailableReasons)
	}
	if !slices.Contains(provenance.UnavailableReasons, CompileProvenanceReasonExternalAssetIdentityMissing) {
		t.Fatalf("UnavailableReasons = %v, want stable external-asset identity gap", provenance.UnavailableReasons)
	}
	wantPromptHash := sha256.Sum256([]byte(prompt))
	if !slices.ContainsFunc(provenance.ExternalAssets, func(asset ExternalAssetRead) bool {
		return filepath.Clean(asset.Path) == filepath.Join(dir, "prompt.md") && asset.SHA256 == hex.EncodeToString(wantPromptHash[:])
	}) {
		t.Fatalf("external reads do not bind description bytes: %#v", provenance.ExternalAssets)
	}
}

func TestCompileWithProvenanceMarksUnresolvedDescriptionUnavailable(t *testing.T) {
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "missing-description.toml"), `formula = "missing-description"
version = 1

[[steps]]
id = "task"
title = "Task"
description_file = "missing.md"
`)

	_, provenance, err := CompileWithProvenance(context.Background(), "missing-description", []string{dir}, nil)
	if err != nil {
		t.Fatalf("CompileWithProvenance: %v", err)
	}
	if provenance.Status != CompileProvenanceUnavailable || !slices.Contains(provenance.UnavailableReasons, CompileProvenanceReasonExternalAssetBytesUnavailable) {
		t.Fatalf("provenance = %#v, want unavailable external asset bytes", provenance)
	}
}

func TestCompileWithProvenancePreservesInlineExpansionUseOrder(t *testing.T) {
	enableV2ForTest(t)
	t.Setenv("GC_FORMULA_REF", "")

	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "root.toml"), `formula = "root"
version = 1

[vars.mode]
default = "fast"

[[steps]]
id = "work"
title = "Work"
expand = "attempts"
expand_vars = { mode = "{{mode}}" }
`)
	writeFormulaTestFile(t, filepath.Join(dir, "attempts.toml"), `formula = "attempts"
version = 1
type = "expansion"

[vars.mode]
default = "slow"

[[template]]
id = "{target}.attempt"
title = "Attempt {{mode}}"
`)

	inputVars := map[string]string{"mode": "fast"}
	recipe, provenance, err := CompileWithProvenance(context.Background(), "root", []string{dir}, inputVars)
	if err != nil {
		t.Fatalf("CompileWithProvenance: %v", err)
	}
	if provenance.Status != CompileProvenanceAvailable {
		t.Fatalf("Status = %q, want available: %v", provenance.Status, provenance.UnavailableReasons)
	}
	if len(provenance.Trace) != 1 {
		t.Fatalf("Trace = %#v, want one inline expansion event", provenance.Trace)
	}
	entry := provenance.Trace[0]
	if entry.Kind != CompileTraceInlineExpansion || entry.FormulaName != "attempts" || entry.TargetStepID != "work" {
		t.Fatalf("inline expansion trace = %#v, want attempts on work", entry)
	}
	if got := entry.EffectiveVariables["mode"]; got != "fast" {
		t.Fatalf("expansion effective mode = %q, want fast", got)
	}
	inputVars["mode"] = "mutated-input"
	entry.EffectiveVariables["mode"] = "mutated-trace"
	if got := provenance.EffectiveCompileVariables["mode"]; got != "fast" {
		t.Fatalf("effective vars changed through shared map: %q", got)
	}
	provenance.FormulaSources[0].FormulaName = "mutated-provenance"
	if got := recipe.FormulaSources[0].FormulaName; got == "mutated-provenance" {
		t.Fatal("provenance FormulaSources shares backing storage with Recipe.FormulaSources")
	}
}

func TestCompileWithoutRuntimeVarValidationWithProvenancePreservesDeferredChecks(t *testing.T) {
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "deferred.toml"), `formula = "deferred"
version = 1

[vars.required_at_runtime]
required = true

[[steps]]
id = "task"
title = "Task"
`)
	vars := map[string]string{"unrelated": "value"}

	if _, err := Compile(context.Background(), "deferred", []string{dir}, vars); err == nil {
		t.Fatal("Compile succeeded without a required runtime variable, want validation error")
	}
	plain, err := CompileWithoutRuntimeVarValidation(context.Background(), "deferred", []string{dir}, vars)
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidation: %v", err)
	}
	withProvenance, provenance, err := CompileWithoutRuntimeVarValidationWithProvenance(context.Background(), "deferred", []string{dir}, vars)
	if err != nil {
		t.Fatalf("CompileWithoutRuntimeVarValidationWithProvenance: %v", err)
	}
	if len(plain.Steps) != len(withProvenance.Steps) {
		t.Fatalf("step count changed: plain=%d provenance=%d", len(plain.Steps), len(withProvenance.Steps))
	}
	if provenance.Status != CompileProvenanceAvailable {
		t.Fatalf("Status = %q, want available: %v", provenance.Status, provenance.UnavailableReasons)
	}
	if got := provenance.EffectiveCompileVariables["unrelated"]; got != "value" {
		t.Fatalf("effective unrelated variable = %q, want value", got)
	}
}

func TestCompileWithProvenanceCapturesDisabledV2Capability(t *testing.T) {
	previous := IsFormulaV2Enabled()
	SetFormulaV2Enabled(false)
	t.Cleanup(func() { SetFormulaV2Enabled(previous) })
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "legacy.toml"), `formula = "legacy"
version = 1

[[steps]]
id = "task"
title = "Task"
`)

	_, provenance, err := CompileWithProvenance(context.Background(), "legacy", []string{dir}, nil)
	if err != nil {
		t.Fatalf("CompileWithProvenance: %v", err)
	}
	if provenance.FormulaV2Enabled {
		t.Fatal("FormulaV2Enabled = true, want false")
	}
	if provenance.CompilerCapability != defaultFormulaCompilerCapability {
		t.Fatalf("CompilerCapability = %q, want %q", provenance.CompilerCapability, defaultFormulaCompilerCapability)
	}
}

func TestCompileImplementationVersionIsSeparateFromRequirementCapability(t *testing.T) {
	previous := IsFormulaV2Enabled()
	t.Cleanup(func() { SetFormulaV2Enabled(previous) })
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "legacy.toml"), `formula = "legacy"
version = 1

[[steps]]
id = "task"
title = "Task"
`)

	var implementationVersions []string
	var capabilities []string
	for _, v2Enabled := range []bool{false, true} {
		SetFormulaV2Enabled(v2Enabled)
		_, provenance, err := CompileWithProvenance(context.Background(), "legacy", []string{dir}, nil)
		if err != nil {
			t.Fatalf("CompileWithProvenance with v2=%t: %v", v2Enabled, err)
		}
		implementationVersions = append(implementationVersions, provenance.CompilerImplementationVersion)
		capabilities = append(capabilities, provenance.CompilerCapability)
		if provenance.CompilerImplementationVersion != FormulaCompilerImplementationVersion {
			t.Fatalf("CompilerImplementationVersion = %q, want %q", provenance.CompilerImplementationVersion, FormulaCompilerImplementationVersion)
		}
		if provenance.CompilerImplementationVersion == provenance.CompilerCapability {
			t.Fatalf("implementation version and requirement capability must be distinct: %q", provenance.CompilerCapability)
		}
	}
	if !slices.Equal(implementationVersions, []string{FormulaCompilerImplementationVersion, FormulaCompilerImplementationVersion}) {
		t.Fatalf("implementation versions = %v, want stable compiler implementation identity", implementationVersions)
	}
	if !slices.Equal(capabilities, []string{defaultFormulaCompilerCapability, currentFormulaCompilerCapability}) {
		t.Fatalf("requirement capabilities = %v, want legacy then formula-v2 capability", capabilities)
	}
}

func TestCompileWithProvenanceRecordsComposeAndAspectOrder(t *testing.T) {
	enableV2ForTest(t)
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "root.toml"), `formula = "root"
version = 1

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Work"

[compose]
aspects = ["audit"]

[[compose.expand]]
target = "work"
with = "decorate"
`)
	writeFormulaTestFile(t, filepath.Join(dir, "decorate.toml"), `formula = "decorate"
version = 1
type = "expansion"

[[template]]
id = "{target}.step"
title = "Do the work"
`)
	writeFormulaTestFile(t, filepath.Join(dir, "audit.toml"), `formula = "audit"
version = 1
type = "aspect"

[requires]
formula_compiler = ">=2.0.0"

[[advice]]
target = "work"
after = { id = "{step.id}.audit", title = "Audit" }
`)

	_, provenance, err := CompileWithProvenance(context.Background(), "root", []string{dir}, nil)
	if err != nil {
		t.Fatalf("CompileWithProvenance: %v", err)
	}
	if len(provenance.Trace) != 2 || provenance.Trace[0].Kind != CompileTraceComposeExpand || provenance.Trace[1].Kind != CompileTraceAspect {
		t.Fatalf("Trace = %#v, want compose expansion followed by aspect", provenance.Trace)
	}
	if provenance.Trace[0].FormulaName != "decorate" || provenance.Trace[0].TargetStepID != "work" || provenance.Trace[1].FormulaName != "audit" {
		t.Fatalf("Trace = %#v, want actual formula identities and target", provenance.Trace)
	}
}

func TestCompileWithProvenanceComposeMapTargetOrderIsDeterministic(t *testing.T) {
	enableV2ForTest(t)
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	writeFormulaTestFile(t, filepath.Join(dir, "root.toml"), `formula = "root"
version = 1

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "impl.zeta"
title = "Zeta"

[[steps]]
id = "impl.alpha"
title = "Alpha"

[[steps]]
id = "impl.mu"
title = "Mu"

[[compose.map]]
select = "impl.*"
with = "decorate"
`)
	writeFormulaTestFile(t, filepath.Join(dir, "decorate.toml"), `formula = "decorate"
version = 1
type = "expansion"

[[template]]
id = "{target}.decorated"
title = "Decorated {target.title}"
`)

	want := []string{"impl.alpha", "impl.mu", "impl.zeta"}
	for attempt := 0; attempt < 12; attempt++ {
		_, provenance, err := CompileWithProvenance(context.Background(), "root", []string{dir}, nil)
		if err != nil {
			t.Fatalf("CompileWithProvenance attempt %d: %v", attempt, err)
		}
		var got []string
		for _, entry := range provenance.Trace {
			if entry.Kind == CompileTraceComposeMap {
				got = append(got, entry.TargetStepID)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("attempt %d compose-map trace targets = %v, want stable order %v", attempt, got, want)
		}
	}
}

func TestCompileProvenanceRejectsAmbiguousNamesAndUnboundReads(t *testing.T) {
	recorder := newCompileProvenanceRecorder(FSSource{})
	recorder.recordFormula(SourceIdentity{Path: "/one/shared.toml", FormulaName: "shared", ContentSHA256: strings.Repeat("a", 64)})
	recorder.recordFormula(SourceIdentity{Path: "/two/shared.toml", FormulaName: "shared", ContentSHA256: strings.Repeat("b", 64)})
	recorder.recordRead("/outside/unknown.dat", []byte("unknown"), compileSourceReadInfo{mode: CompileSourceReadModeFilesystem})
	provenance := recorder.finish(&Recipe{}, nil, true)
	if provenance.Status != CompileProvenanceUnavailable {
		t.Fatalf("Status = %q, want unavailable", provenance.Status)
	}
	for _, reason := range []CompileProvenanceReason{CompileProvenanceReasonAmbiguousFormulaName, CompileProvenanceReasonUnclassifiedRead} {
		if !slices.Contains(provenance.UnavailableReasons, reason) {
			t.Errorf("UnavailableReasons = %v, missing %q", provenance.UnavailableReasons, reason)
		}
	}
}

func TestCompileProvenanceRejectsChangedBytesForRepeatedPath(t *testing.T) {
	recorder := newCompileProvenanceRecorder(FSSource{})
	recorder.recordRead("/formula.toml", []byte("first"), compileSourceReadInfo{mode: CompileSourceReadModeFilesystem})
	recorder.recordRead("/formula.toml", []byte("second"), compileSourceReadInfo{mode: CompileSourceReadModeFilesystem})
	provenance := recorder.finish(&Recipe{}, nil, true)
	if !slices.Contains(provenance.UnavailableReasons, CompileProvenanceReasonInconsistentRead) {
		t.Fatalf("UnavailableReasons = %v, want inconsistent-read reason", provenance.UnavailableReasons)
	}
}

func TestRecordingSourceReportsGitRefAndFilesystemFallbackPerRead(t *testing.T) {
	gitSource := gitRepoAwareFallback{git: NewGitRefSource("HEAD"), fs: FSSource{}}
	recorder := newCompileProvenanceRecorder(gitSource)
	reader := recordingFormulaSource{Source: gitSource, recorder: recorder}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadFile(filepath.Join(root, "internal", "formula", "compile.go")); err != nil {
		t.Fatalf("read repository file at ref: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "external.txt")
	writeFormulaTestFile(t, outside, "filesystem fallback")
	if _, err := reader.ReadFile(outside); err != nil {
		t.Fatalf("read fallback file: %v", err)
	}
	if recorder.descriptor.Mode != CompileSourceModeGitRefFallback || recorder.descriptor.Ref != "HEAD" {
		t.Fatalf("source descriptor = %#v, want git-ref fallback at HEAD", recorder.descriptor)
	}
	if len(recorder.reads) != 2 || recorder.reads[0].Mode != CompileSourceReadModeGitRef || recorder.reads[0].Ref != "HEAD" ||
		recorder.reads[1].Mode != CompileSourceReadModeFilesystem || recorder.reads[1].Ref != "HEAD" {
		t.Fatalf("per-read source facts = %#v, want git-ref then filesystem fallback", recorder.reads)
	}
}

type provenanceTestSource struct {
	files map[string][]byte
}

func (s provenanceTestSource) Stat(path string) bool {
	_, ok := s.files[filepath.Clean(path)]
	return ok
}

func (s provenanceTestSource) ReadFile(path string) ([]byte, error) {
	data, ok := s.files[filepath.Clean(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (s provenanceTestSource) ListDir(string) ([]string, error) {
	return nil, nil
}

func TestCompileWithUnknownSourceModeIsUnavailable(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "virtual-formulas")
	formulaPath := filepath.Join(dir, "virtual.toml")
	source := provenanceTestSource{files: map[string][]byte{
		formulaPath: []byte(`formula = "virtual"
version = 1

[[steps]]
id = "task"
title = "Task"
`),
	}}
	recorder := newCompileProvenanceRecorder(source)
	_, err := compileFormulaWithSource("virtual", []string{dir}, nil, true, source, recorder)
	if err != nil {
		t.Fatalf("compile with source: %v", err)
	}
	if recorder.result.Status != CompileProvenanceUnavailable || !slices.Contains(recorder.result.UnavailableReasons, CompileProvenanceReasonUnknownSourceMode) {
		t.Fatalf("provenance = %#v, want unknown-source unavailable", recorder.result)
	}
}

func writeFormulaTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func formulaNames(sources []SourceIdentity) []string {
	names := make([]string, 0, len(sources))
	for _, source := range sources {
		names = append(names, source.FormulaName)
	}
	slices.Sort(names)
	return names
}
