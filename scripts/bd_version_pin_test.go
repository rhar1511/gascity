package scripts_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/gastownhall/gascity/internal/deps"
)

// TestBDVersionPins keeps upstream installation/matrix floors in sync and
// validates the separate direct native-library anchors. Before this test the
// bd floors drifted apart: deps.env BD_VERSION, the init hard-dependency floor
// bdMinVersion, the ready-projection feature floor bdReadyProjectionMinVersion,
// the bd_compatibility config enum, and the install-bd-archive.sh SHA table were
// all hand-edited with no cross-check, so a regression like #3135 (a 1.0.5 flag
// emitted ahead of the pinned 1.0.4 floor) could merge green. This test makes
// deps.env the single source of truth and fails loudly the moment an anchor
// moves without the others.
func TestBDVersionPins(t *testing.T) {
	t.Run("qualified native CLI installer protocol", testQualifiedNativeBDInstallerProtocol)
	t.Run("native CI pair wiring", testNativeCIPairWiring)
	t.Run("native mail fixture input capture", testNativeMailFixtureInputCapture)
	t.Run("native rig worktree fixture selection", testNativeRigWorktreeFixtureSelection)
	t.Run("native Bazel integration selection", testNativeBazelIntegrationSelection)
	root := repoRoot(t)
	env := readDotenv(t, filepath.Join(root, "deps.env"))

	bdVersion := env["BD_VERSION"]         // installable default (v-prefixed release tag)
	bdPrev := env["BD_PREV_VERSION"]       // min-supported matrix cell (downloadable)
	bdCurrent := env["BD_CURRENT_VERSION"] // bleeding-edge matrix cell (built from source)
	bdCurrentRef := env["BD_CURRENT_REF"]  // beads commit the current cell builds from

	if bdVersion == "" {
		t.Fatal("deps.env missing BD_VERSION")
	}
	if bdPrev == "" {
		t.Fatal("deps.env missing BD_PREV_VERSION (the minimum-supported contract-matrix cell)")
	}
	if bdCurrent == "" {
		t.Fatal("deps.env missing BD_CURRENT_VERSION (the bleeding-edge contract-matrix cell)")
	}

	// The current cell is built from a pinned beads commit, which need not be a
	// tagged release. A non-deterministic ref (branch name, short SHA) would make
	// the cell irreproducible; require a full 40-char commit SHA.
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(bdCurrentRef) {
		t.Fatalf("deps.env BD_CURRENT_REF = %q, want a full 40-char gastownhall/beads commit SHA", bdCurrentRef)
	}
	if !regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(bdCurrent) {
		t.Fatalf("deps.env BD_CURRENT_VERSION = %q, want a semver token", bdCurrent)
	}
	// The source-built upstream CLI matrix and image stay coupled below. The
	// linked owner library has its own module/version/source anchors; a CLI
	// distribution's version string is not its source provenance.
	goMod := readFile(t, root, "go.mod")
	goModPin, err := validateBeadsLibraryPins(env, goMod)
	if err != nil {
		t.Fatal(err)
	}
	// The integration suite installs bd from whatever go.mod names and pins the
	// expected version in its own literal, so a bump that misses that literal
	// leaves the suite asserting a version nobody ships. It lives in the
	// `rest-full` shard, which is gated on `push` — so on a PR nothing catches
	// the drift and the failure lands after merge, which is exactly how
	// v1.3.0-rc.2 stayed stale there (tracker ga-rnwg5u). Assert it here,
	// against the same go.mod pin the block above ties to deps.env. This test
	// reaches PR-time CI through `make test-ci-policy` (preflight-static); the
	// ./scripts unit-cover jobs are push-only, so that recipe line is what
	// makes this fail before merge rather than after —
	// TestMakeTestCIPolicyRunsVersionPinContracts pins it.
	const integrationPinFile = "test/integration/integration_test.go"
	integrationPin := extractGoStringConst(t, root, integrationPinFile, "wantPinnedBeadsModuleVersion")
	if integrationPin == "" {
		t.Fatalf("%s missing the wantPinnedBeadsModuleVersion const; it is the integration suite's beads pin anchor", integrationPinFile)
	}
	if integrationPin != goModPin {
		t.Fatalf("%s pins wantPinnedBeadsModuleVersion = %q but go.mod pins the owner Beads library to %q; the integration suite installs bd from go.mod, so the two must name the same version",
			integrationPinFile, integrationPin, goModPin)
	}

	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	if !strings.Contains(dockerfile, "ARG BD_SOURCE_REF="+bdCurrentRef) {
		t.Fatalf("contrib/k8s/Dockerfile.agent BD_SOURCE_REF must equal deps.env BD_CURRENT_REF (%s)", bdCurrentRef)
	}
	if !strings.Contains(dockerfile, "ARG BD_BUILD="+bdCurrentRef[:10]) {
		t.Fatalf("contrib/k8s/Dockerfile.agent BD_BUILD must equal the first 10 characters of BD_CURRENT_REF (%s)", bdCurrentRef[:10])
	}

	// Anchor roles, kept as distinct contracts so a promotion cannot quietly
	// collapse them:
	//   BD_PREV_VERSION -- the minimum-supported bd (the matrix floor cell).
	//   BD_VERSION      -- the installable default; must be >= the floor.
	// The init hard-dependency floor (bdMinVersion) is the minimum-supported
	// version restated as a Go constant, so it must track BD_PREV_VERSION, not
	// BD_VERSION. Tying it to BD_VERSION would drag the hard floor up the moment
	// BD_VERSION is promoted (e.g. -> v1.0.5) and drop support for the
	// min-supported matrix cell these contract tests exist to keep green.
	bdMin := extractGoStringConst(t, root, "cmd/gc/init_provider_readiness.go", "bdMinVersion")
	if bdMin != strings.TrimPrefix(bdPrev, "v") {
		t.Fatalf("bdMinVersion = %q but deps.env BD_PREV_VERSION = %q (want %q); the init hard floor is the minimum-supported bd and must track BD_PREV_VERSION, not BD_VERSION",
			bdMin, bdPrev, strings.TrimPrefix(bdPrev, "v"))
	}
	// The installable default may move ahead of the floor but never behind it.
	if deps.CompareVersions(bdVersion, bdPrev) < 0 {
		t.Fatalf("deps.env BD_VERSION = %q is older than BD_PREV_VERSION = %q; the installable default must be at least the minimum-supported version",
			bdVersion, bdPrev)
	}

	// The ready-projection feature floor (#3135's regressing surface) must exist
	// and be strictly newer than the init floor, otherwise the gated path is dead
	// for every supported bd. Compare semantically -- the same way the runtime
	// gate in bdstore_ready_projection.go does (deps.CompareVersions) -- so a
	// floor that is merely different from the init floor, including an older one,
	// cannot pass.
	readyFloor := extractGoStringConst(t, root, "internal/beads/bdstore_ready_projection.go", "bdReadyProjectionMinVersion")
	if readyFloor == "" {
		t.Fatal("internal/beads/bdstore_ready_projection.go missing bdReadyProjectionMinVersion const")
	}
	if deps.CompareVersions(readyFloor, bdMin) <= 0 {
		t.Fatalf("bdReadyProjectionMinVersion (%q) must be strictly newer than bdMinVersion (%q); a feature floor at or below the init floor gates nothing", readyFloor, bdMin)
	}

	// The fresh provider-owned floor is the third anchor: a scope initialized
	// through bd's persisted ownership/transport contract needs a bd that has
	// it. Like the ready-projection floor it must be strictly newer than the
	// init floor (otherwise it gates nothing), and unlike it, it must also be
	// reachable -- no supported bd could satisfy a floor above the newest
	// matrix cell, so a fresh `gc init` would refuse on every cell.
	// The current cell is the v1.3.0 tag, which compares equal to the 1.3.0
	// floor. deps.CompareVersions strips prerelease identifiers, so a
	// v1.3.0-rc.N cell compares equal too rather than below it.
	freshProviderFloor := extractGoStringConst(t, root, "cmd/gc/init_provider_readiness.go", "bdFreshProviderMinVersion")
	if freshProviderFloor == "" {
		t.Fatal("cmd/gc/init_provider_readiness.go missing bdFreshProviderMinVersion const")
	}
	if deps.CompareVersions(freshProviderFloor, bdMin) <= 0 {
		t.Fatalf("bdFreshProviderMinVersion (%q) must be strictly newer than bdMinVersion (%q); a feature floor at or below the init floor gates nothing", freshProviderFloor, bdMin)
	}
	if deps.CompareVersions(freshProviderFloor, bdCurrent) > 0 {
		t.Fatalf("bdFreshProviderMinVersion (%q) is newer than deps.env BD_CURRENT_VERSION (%q); no supported bd could initialize a fresh provider-owned scope",
			freshProviderFloor, bdCurrent)
	}

	// The bd_compatibility config enum is the operator-facing mirror of the two
	// floors; both floor values must appear as enum members so they cannot diverge.
	cfg := readFile(t, root, "internal/config/config.go")
	for _, member := range []string{"enum=bd-" + bdMin, "enum=bd-" + readyFloor} {
		if !strings.Contains(cfg, member) {
			t.Fatalf("internal/config/config.go bd_compatibility enum missing %q (floors: init=%s ready=%s)", member, bdMin, readyFloor)
		}
	}

	// Every released bd that the required CI paths install from a tarball must
	// carry a pinned SHA for every os/arch, never the API fallback. That is both
	// the minimum-supported cell (BD_PREV_VERSION) and the installable default
	// (BD_VERSION): they are the same value today but may diverge on a promotion,
	// and main CI installs BD_VERSION directly. Deduplicate when they are equal.
	install := readFile(t, root, ".github/scripts/install-bd-archive.sh")
	requiredReleases := []string{bdPrev}
	if bdVersion != bdPrev {
		requiredReleases = append(requiredReleases, bdVersion)
	}
	for _, release := range requiredReleases {
		for _, tuple := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
			want := release + ":" + tuple
			if !strings.Contains(install, want) {
				t.Fatalf(".github/scripts/install-bd-archive.sh missing SHA pin %q; %s cannot install on the required path without it", want, release)
			}
		}
	}

	// Every workflow that pins BD_VERSION must pin the same value as deps.env, so a
	// bump in one place cannot leave a stale matrix cell behind. Validate every
	// assignment in both .yml and .yaml workflows: a file-level presence check
	// would let a stale pin ride along beside a correct one.
	assertWorkflowPins(t, root, "BD_VERSION", bdVersion)

	// The devcontainer README restates the installed version in prose, which
	// makes it an anchor like any other -- and it was the only one no test read,
	// so it sat at v1.0.4 through the promotion to v1.1.0. A doc anchor nothing
	// asserts is how the next bump goes half-applied.
	assertDocPinAnchor(t, root, ".devcontainer/README.md", "BD_VERSION", bdVersion)

	// The fresh-init floor is the one an operator meets as a typed refusal from
	// `gc init`, so the design note that explains it is an anchor too. It went
	// stale once already -- the note said proxied was the default "on bd >=
	// 1.3.0" while the code refused outright below it.
	assertDocStatesVersion(t, root, "engdocs/design/beads-proxied-local-default.md",
		"bd ≥ "+freshProviderFloor, "the fresh provider-owned init floor")
}

// validateBeadsLibraryPins checks the direct stable library separately from
// external CLI distributions. Public tag-to-source provenance is verified by
// release qualification; this offline check rejects missing or divergent pins.
func validateBeadsLibraryPins(env map[string]string, goMod string) (string, error) {
	const modulePath = "github.com/rhar1511/beads"
	if env["BEADS_LIBRARY_MODULE"] != modulePath {
		return "", fmt.Errorf("BEADS_LIBRARY_MODULE must be %s", modulePath)
	}
	version := env["BEADS_LIBRARY_VERSION"]
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return "", fmt.Errorf("BEADS_LIBRARY_VERSION must name a stable release, got %q", version)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(env["BEADS_LIBRARY_REF"]) {
		return "", fmt.Errorf("BEADS_LIBRARY_REF must be the full immutable release-source commit")
	}
	file, err := modfile.Parse("go.mod", []byte(goMod), nil)
	if err != nil {
		return "", fmt.Errorf("parse direct library pin: %w", err)
	}
	found := 0
	for _, required := range file.Require {
		if required.Mod.Path == "github.com/steveyegge/beads" {
			return "", fmt.Errorf("stale upstream Beads requirement remains")
		}
		if required.Mod.Path == modulePath {
			found++
			if required.Mod.Version != version || required.Indirect {
				return "", fmt.Errorf("direct owner library pin %q must equal BEADS_LIBRARY_VERSION %q", required.Mod.Version, version)
			}
		}
	}
	if found != 1 {
		return "", fmt.Errorf("expected exactly one direct %s requirement, got %d", modulePath, found)
	}
	for _, replacement := range file.Replace {
		if replacement.Old.Path == modulePath || replacement.New.Path == modulePath {
			return "", fmt.Errorf("owner library pin must not use a replacement")
		}
	}
	return version, nil
}

func TestBeadsLibraryPinsRejectDrift(t *testing.T) {
	const validMod = "module example.com/test\n\ngo 1.26.0\n\nrequire github.com/rhar1511/beads v1.3.2\n"
	cases := []struct {
		name, key, value, mod string
		wantErr               bool
	}{
		{name: "direct stable library", mod: validMod},
		{name: "wrong distribution", key: "BEADS_LIBRARY_MODULE", value: "github.com/steveyegge/beads", mod: validMod, wantErr: true},
		{name: "missing release", key: "BEADS_LIBRARY_VERSION", mod: validMod, wantErr: true},
		{name: "unreleased pseudo-version", key: "BEADS_LIBRARY_VERSION", value: "v1.3.2-0.20261013010644-3c3423cf09a9", mod: validMod, wantErr: true},
		{name: "prerelease", key: "BEADS_LIBRARY_VERSION", value: "v1.3.2-rc.1", mod: validMod, wantErr: true},
		{name: "unpinned source", key: "BEADS_LIBRARY_REF", value: "main", mod: validMod, wantErr: true},
		{name: "missing source", key: "BEADS_LIBRARY_REF", mod: validMod, wantErr: true},
		{name: "version drift", key: "BEADS_LIBRARY_VERSION", value: "v1.3.3", mod: validMod, wantErr: true},
		{name: "missing module", mod: "module example.com/test\n", wantErr: true},
		{name: "stale upstream also present", mod: validMod + "require github.com/steveyegge/beads v1.3.1\n", wantErr: true},
		{name: "indirect library", mod: strings.Replace(validMod, "v1.3.2\n", "v1.3.2 // indirect\n", 1), wantErr: true},
		{name: "replacement", mod: validMod + "replace github.com/rhar1511/beads => example.com/other v1.3.2\n", wantErr: true},
		{name: "replacement target", mod: validMod + "replace example.com/other => github.com/rhar1511/beads v1.3.2\n", wantErr: true},
		{name: "malformed go.mod", mod: "invalid module content", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"BEADS_LIBRARY_MODULE": "github.com/rhar1511/beads", "BEADS_LIBRARY_VERSION": "v1.3.2", "BEADS_LIBRARY_REF": "11863eb43b8807adaa4aa0f2e9b8fd4aeb83f624"}
			if tc.key != "" {
				env[tc.key] = tc.value
			}
			version, err := validateBeadsLibraryPins(env, tc.mod)
			if (err != nil) != tc.wantErr {
				t.Fatalf("version=%q error=%v, want error=%v", version, err, tc.wantErr)
			}
			if err == nil && version != "v1.3.2" {
				t.Fatalf("version=%q, want v1.3.2", version)
			}
		})
	}
}

// assertDocStatesVersion fails when a doc no longer restates a version floor in
// prose. Unlike assertDocPinAnchor it matches a literal phrase rather than a
// deps.env restatement, because this floor is a Go const, not a deps.env key.
func assertDocStatesVersion(t *testing.T, root, rel, phrase, what string) {
	t.Helper()
	if !strings.Contains(readFile(t, root, rel), phrase) {
		t.Errorf("%s no longer states %s as %q; update the doc or move this assertion with it", rel, what, phrase)
	}
}

// assertDocPinAnchor fails when a doc restates a deps.env pin as
// "`KEY` from `deps.env` (currently VALUE)" and VALUE has drifted. The phrasing
// is the contract: prose that names the variable without restating the value is
// not an anchor and is not matched, so a doc can always opt out by dropping the
// parenthetical rather than by going stale.
func assertDocPinAnchor(t *testing.T, root, rel, key, want string) {
	t.Helper()
	re := regexp.MustCompile("`" + regexp.QuoteMeta(key) + "` from `deps\\.env` \\(currently ([^)]+)\\)")
	m := re.FindStringSubmatch(readFile(t, root, rel))
	if m == nil {
		t.Fatalf("%s no longer restates %s as \"`%s` from `deps.env` (currently <value>)\"; either restore that phrasing or drop this assertion with the anchor", rel, key, key)
	}
	if got := strings.TrimSpace(m[1]); got != want {
		t.Errorf("%s says %s is currently %q, want %q (deps.env)", rel, key, got, want)
	}
}

// TestScanPinAssignments proves the workflow pin scanner catches the partial
// drift a file-level presence check missed: a stale BD_VERSION sharing a file
// with a correct one is still reported with its line, while a
// `${{ env.BD_VERSION }}` reference is not treated as an assignment.
func TestScanPinAssignments(t *testing.T) {
	const fixture = `env:
  BD_VERSION: "v1.0.4"
  DOLT_VERSION: "2.1.7"
jobs:
  stale:
    env:
      BD_VERSION: "v1.0.3"
    steps:
      - with:
          bd-version: ${{ env.BD_VERSION }}
`
	got := scanPinAssignments("BD_VERSION", fixture)
	want := []pinAssignment{
		{line: 2, value: "v1.0.4"},
		{line: 7, value: "v1.0.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanPinAssignments(BD_VERSION) = %+v, want %+v", got, want)
	}
}

// readDotenv parses simple KEY=VALUE lines, ignoring comments and blanks.
func readDotenv(t *testing.T, path string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(content)
}

// extractGoStringConst returns the value of a `name = "..."` Go string constant,
// or "" if the file does not declare it. The pattern is anchored to a real
// declaration form -- the identifier must start a line, optionally preceded by
// indentation and the `const` keyword -- so a comment or prose example naming the
// same identifier above the real const cannot be matched first.
func extractGoStringConst(t *testing.T, root, rel, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]+)"`)
	m := re.FindStringSubmatch(readFile(t, root, rel))
	if m == nil {
		return ""
	}
	return m[1]
}

// pinAssignment is a single `KEY: value` mapping entry found in a workflow file,
// carrying its 1-based line number for diagnostics.
type pinAssignment struct {
	line  int
	value string
}

// scanPinAssignments returns every `key: value` assignment in content. It matches
// only a mapping key -- optional indentation, the exact key, then a colon -- so a
// reference such as `bd-version: ${{ env.BD_VERSION }}` is not mistaken for an
// assignment of BD_VERSION. Surrounding quotes and any trailing comment are
// stripped from the captured value.
func scanPinAssignments(key, content string) []pinAssignment {
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `:\s*["']?([^"'\s#]+)["']?`)
	var out []pinAssignment
	for i, line := range strings.Split(content, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			out = append(out, pinAssignment{line: i + 1, value: m[1]})
		}
	}
	return out
}

// assertWorkflowPins fails for every workflow assignment of key whose value is not
// want, scanning both .yml and .yaml workflows and reporting each offending file
// and line. Validating every assignment -- not just file-level presence -- catches
// a file that mixes a correct pin with a stale one, and reporting via t.Errorf
// rather than t.Fatalf surfaces all stale pins in a single run.
func assertWorkflowPins(t *testing.T, root, key, want string) {
	t.Helper()
	dir := filepath.Join(root, ".github", "workflows")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, a := range scanPinAssignments(key, string(content)) {
			if a.value != want {
				t.Errorf("%s:%d pins %s to %q, want %q (deps.env)", rel, a.line, key, a.value, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workflows: %v", err)
	}
}

// testQualifiedNativeBDInstallerProtocol exercises the real installer with a
// deterministic Go/tool protocol. These doubles prove selection and refusal,
// not release authenticity or native schema parity; those remain real CI gates.
func testQualifiedNativeBDInstallerProtocol(t *testing.T) {
	root := repoRoot(t)
	installer := readFile(t, root, ".github/scripts/install-qualified-integration-bd.sh")
	const module = "github.com/rhar1511/beads"
	const version = "v1.3.2"
	const ref = "11863eb43b8807adaa4aa0f2e9b8fd4aeb83f624"
	const sum = "h1:aQ6Di/V7qmK8F54HJhYMn0j3qCbUiHI7UdwEcp+EsNc="
	const modSum = "h1:3GXUkW2Ju+qM/wiuj8XT6owIVUZzUkBZtdAIC4IVid4="
	type scenario struct {
		name, os, arch, pinModule, pinVersion, pinRef, sumText string
		consumer, resolved, downloaded, buildinfo              string
		graphBefore, graphAfter                                string
		buildFail, smokeFail, crossHost, wantFailure           bool
		resolveFail, initialModDrift, modDrift, stageSumDrift  bool
	}
	consumer := `{"Require":[{"Path":"` + module + `","Version":"` + version + `"}],"Replace":null}`
	resolved := `{"Path":"` + module + `","Version":"` + version + `","Sum":"` + sum + `"}`
	download := `{"Path":"` + module + `","Version":"` + version + `","Sum":"` + sum + `","GoModSum":"` + modSum + `"}`
	buildinfo := "\tpath\t" + module + "/cmd/bd\n\tmod\t" + module + "\t" + version + "\t" + sum + "\n\tbuild\tCGO_ENABLED=1\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n"
	sumText := module + " " + version + " " + sum + "\n" + module + " " + version + "/go.mod " + modSum + "\n"
	graph := `{"Path":"example.com/consumer","Main":true}` + "\n" + resolved + "\n" + `{"Path":"example.com/existing","Version":"v1.2.3"}` + "\n"
	cases := []scenario{
		{name: "released Linux native pair"},
		{name: "released Darwin native pair", os: "darwin", arch: "arm64"},
		{name: "unknown distribution", pinModule: "github.com/steveyegge/beads", wantFailure: true},
		{name: "unknown release", pinVersion: "v1.3.3", wantFailure: true},
		{name: "unknown source", pinRef: strings.Repeat("a", 40), wantFailure: true},
		{name: "wrong consumer zip sum", sumText: strings.ReplaceAll(sumText, sum, "h1:wrong"), wantFailure: true},
		{name: "missing consumer gomod sum", sumText: module + " " + version + " " + sum + "\n", wantFailure: true},
		{name: "duplicate consumer sum", sumText: sumText + module + " " + version + " " + sum + "\n", wantFailure: true},
		{name: "consumer version drift", consumer: strings.ReplaceAll(consumer, version, "v1.3.1"), wantFailure: true},
		{name: "indirect consumer", consumer: strings.Replace(consumer, `"Version":"`+version+`"`, `"Version":"`+version+`","Indirect":true`, 1), wantFailure: true},
		{name: "local replacement", consumer: strings.Replace(consumer, `"Replace":null`, `"Replace":[{"Old":{"Path":"`+module+`"},"New":{"Path":"../beads"}}]`, 1), wantFailure: true},
		{name: "remote replacement", consumer: strings.Replace(consumer, `"Replace":null`, `"Replace":[{"Old":{"Path":"`+module+`"},"New":{"Path":"example.com/beads","Version":"v1.3.2"}}]`, 1), wantFailure: true},
		{name: "resolved replacement", resolved: strings.TrimSuffix(resolved, "}") + `,"Replace":{"Path":"../beads"}}`, wantFailure: true},
		{name: "resolved version drift", resolved: strings.ReplaceAll(resolved, version, "v1.3.1"), wantFailure: true},
		{name: "resolved sum drift", resolved: strings.ReplaceAll(resolved, sum, "h1:wrong"), wantFailure: true},
		{name: "download sum drift", downloaded: strings.ReplaceAll(download, sum, "h1:wrong"), wantFailure: true},
		{name: "download gomod drift", downloaded: strings.ReplaceAll(download, modSum, "h1:wrong"), wantFailure: true},
		{name: "cross host", crossHost: true, wantFailure: true},
		{name: "build failure", buildFail: true, wantFailure: true},
		{name: "devel binary", buildinfo: strings.ReplaceAll(buildinfo, version+"\t"+sum, "(devel)"), wantFailure: true},
		{name: "binary module mismatch", buildinfo: strings.ReplaceAll(buildinfo, module, "github.com/steveyegge/beads"), wantFailure: true},
		{name: "binary checksum mismatch", buildinfo: strings.ReplaceAll(buildinfo, sum, "h1:wrong"), wantFailure: true},
		{name: "binary replacement", buildinfo: buildinfo + "\t=>\t../beads\t(devel)\n", wantFailure: true},
		{name: "binary without CGO", buildinfo: strings.ReplaceAll(buildinfo, "CGO_ENABLED=1", "CGO_ENABLED=0"), wantFailure: true},
		{name: "pure go binary", buildinfo: buildinfo + "\tbuild\t-tags=gms_pure_go\n", wantFailure: true},
		{name: "wrong binary host", buildinfo: strings.ReplaceAll(buildinfo, "GOOS=linux", "GOOS=darwin"), wantFailure: true},
		{name: "wrong binary architecture", buildinfo: strings.ReplaceAll(buildinfo, "GOARCH=amd64", "GOARCH=arm64"), wantFailure: true},
		{name: "smoke failure", smokeFail: true, wantFailure: true},
		{name: "CLI package resolution failure", resolveFail: true, wantFailure: true},
		{name: "initial graph go mod drift", initialModDrift: true, wantFailure: true},
		{name: "CLI resolution go mod drift", modDrift: true, wantFailure: true},
		{name: "selected graph version drift", graphAfter: strings.ReplaceAll(graph, "v1.2.3", "v1.2.4"), wantFailure: true},
		{name: "selected graph addition", graphAfter: graph + `{"Path":"example.com/added","Version":"v1.0.0"}`, wantFailure: true},
		{name: "selected graph removal", graphAfter: resolved + "\n", wantFailure: true},
		{name: "initial graph replacement", graphBefore: strings.ReplaceAll(graph, `"Version":"v1.2.3"`, `"Version":"v1.2.3","Replace":{"Path":"../local"}`), wantFailure: true},
		{name: "selected graph replacement", graphAfter: strings.ReplaceAll(graph, `"Version":"v1.2.3"`, `"Version":"v1.2.3","Replace":{"Path":"../local"}`), wantFailure: true},
		{name: "stage release checksum drift", stageSumDrift: true, wantFailure: true},
		{name: "empty initial selected graph", graphBefore: " \n", wantFailure: true},
		{name: "duplicate selected graph module", graphAfter: graph + resolved, wantFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := t.TempDir()
			write := func(rel, text string, mode os.FileMode) string {
				t.Helper()
				path := filepath.Join(fixture, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), mode); err != nil {
					t.Fatal(err)
				}
				return path
			}
			or := func(value, fallback string) string {
				if value != "" {
					return value
				}
				return fallback
			}
			osName, arch := or(tc.os, "linux"), or(tc.arch, "amd64")
			info := or(tc.buildinfo, buildinfo)
			if tc.os != "" {
				info = strings.ReplaceAll(info, "GOOS=linux", "GOOS="+osName)
				info = strings.ReplaceAll(info, "GOARCH=amd64", "GOARCH="+arch)
			}
			write("consumer.json", or(tc.consumer, consumer), 0o644)
			write("resolved.json", or(tc.resolved, resolved), 0o644)
			write("download.json", or(tc.downloaded, download), 0o644)
			write("buildinfo", info, 0o644)
			write("graph-before.json", or(tc.graphBefore, graph), 0o644)
			write("graph-after.json", or(tc.graphAfter, graph), 0o644)
			gomod := "module example.com/consumer\n\ngo 1.26.0\n\nrequire " + module + " " + version + "\n"
			write("repo/go.mod", gomod, 0o644)
			checksums := or(tc.sumText, sumText)
			write("repo/go.sum", checksums, 0o644)
			write("repo/deps.env", "BEADS_LIBRARY_MODULE="+or(tc.pinModule, module)+"\nBEADS_LIBRARY_VERSION="+or(tc.pinVersion, version)+"\nBEADS_LIBRARY_REF="+or(tc.pinRef, ref)+"\n", 0o644)
			script := write("repo/.github/scripts/install-qualified-integration-bd.sh", installer, 0o755)
			write("old/bd", "original archive executable\n", 0o755)
			write("mock-bd", "#!/bin/sh\n[ \"$1\" = version ] || exit 99\n[ \"$SMOKE_FAIL\" = false ] || exit 29\nprintf 'fixture bd\\n'\n", 0o755)
			write("bin/go", `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/calls"
case "$*" in
  'env GOOS'|'env GOARCH') ;;
  *) case "$PWD" in "$FIXTURE/runner-temp"/gascity-qualified-bd.*/consumer) ;; *) echo 'Go module operation escaped the private consumer stage' >&2; exit 96 ;; esac ;;
esac
case "$*" in
  'env GOOS') if [[ "$CROSS_HOST" == true ]]; then echo cross; else echo "$FIXTURE_OS"; fi ;;
  'env GOARCH') echo "$FIXTURE_ARCH" ;;
  'mod edit -json') cat "$FIXTURE/consumer.json" ;;
  'list -mod=readonly -m -json github.com/rhar1511/beads') cat "$FIXTURE/resolved.json" ;;
  'mod download -json github.com/rhar1511/beads@v1.3.2') cat "$FIXTURE/download.json" ;;
  'list -mod=mod -m -json all')
    [[ "$INITIAL_MOD_DRIFT" == false ]] || printf '# changed during initial graph load\n' >> go.mod
    cat "$FIXTURE/graph-before.json" ;;
  'list -mod=mod -deps github.com/rhar1511/beads/cmd/bd')
    [[ "$RESOLVE_FAIL" == false ]] || exit 32
    printf 'example.com/cli-only v1.0.0 h1:fixture-cli-sum\n' >> go.sum
    [[ "$MOD_DRIFT" == false ]] || printf '# changed during CLI resolution\n' >> go.mod
    [[ "$STAGE_SUM_DRIFT" == false ]] || printf 'github.com/rhar1511/beads v1.3.2 h1:wrong\n' >> go.sum
    echo github.com/rhar1511/beads/cmd/bd ;;
  'list -mod=readonly -m -json all') cat "$FIXTURE/graph-after.json" ;;
  'mod verify') ;;
  'build -mod=readonly -trimpath -o '*)
    [[ "$#" == 6 && "$6" == github.com/rhar1511/beads/cmd/bd && "$CGO_ENABLED" == 1 && "$GOWORK" == off && "$GOENV" == off && "$GOTOOLCHAIN" == local && -z "$GOFLAGS" ]] || exit 97
    [[ "$BUILD_FAIL" == false ]] || exit 31
    grep -q '^example.com/cli-only v1.0.0 h1:fixture-cli-sum$' go.sum || { echo 'CLI-only sums were not hydrated in private stage' >&2; exit 95; }
    cp "$FIXTURE/mock-bd" "$5" ;;
  'version -m '*) cat "$FIXTURE/buildinfo" ;;
  *) echo "unexpected Go operation: $*" >&2; exit 98 ;;
esac
`, 0o755)
			write("bin/uname", "#!/bin/sh\ncase \"$1\" in -s) if [ \"$FIXTURE_OS\" = darwin ]; then echo Darwin; else echo Linux; fi ;; -m) if [ \"$FIXTURE_ARCH\" = arm64 ]; then echo arm64; else echo x86_64; fi ;; *) exit 99 ;; esac\n", 0o755)
			write("bin/pkg-config", "#!/bin/sh\ncase \"$1\" in --exists) exit 0 ;; --cflags) echo -I/fixture/icu ;; --libs) echo -licui18n ;; *) exit 99 ;; esac\n", 0o755)
			for _, tool := range []string{"curl", "git", "tar", "brew"} {
				write("bin/"+tool, "#!/bin/sh\necho 'unexpected source/network/Homebrew operation' >&2\nexit 99\n", 0o755)
			}
			for _, dir := range []string{"runner-temp", "cache", "home"} {
				if err := os.MkdirAll(filepath.Join(fixture, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Reuse the package's existing subprocess owner; no new constructor.
			cmd := testCommand("bash", script)
			cmd.Dir = filepath.Join(fixture, "repo")
			cmd.Env = []string{
				"PATH=" + filepath.Join(fixture, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + filepath.Join(fixture, "home"), "FIXTURE=" + fixture,
				"FIXTURE_OS=" + osName, "FIXTURE_ARCH=" + arch,
				fmt.Sprintf("BUILD_FAIL=%t", tc.buildFail), fmt.Sprintf("SMOKE_FAIL=%t", tc.smokeFail), fmt.Sprintf("CROSS_HOST=%t", tc.crossHost),
				fmt.Sprintf("RESOLVE_FAIL=%t", tc.resolveFail), fmt.Sprintf("INITIAL_MOD_DRIFT=%t", tc.initialModDrift), fmt.Sprintf("MOD_DRIFT=%t", tc.modDrift), fmt.Sprintf("STAGE_SUM_DRIFT=%t", tc.stageSumDrift),
				"RUNNER_TEMP=" + filepath.Join(fixture, "runner-temp"), "RUNNER_TOOL_CACHE=" + filepath.Join(fixture, "cache"),
				"GITHUB_ENV=" + filepath.Join(fixture, "github-env"), "GITHUB_PATH=" + filepath.Join(fixture, "github-path"),
				"GOFLAGS=-tags=gms_pure_go", "GOWORK=/ambient/go.work", "GOENV=/ambient/goenv",
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantFailure {
				t.Fatalf("installer error=%v, want failure=%v\n%s", err, tc.wantFailure, out)
			}
			selected := filepath.Join(fixture, "cache", "gascity-qualified-bd", ref, osName+"-"+arch, "bd")
			for _, path := range []string{"github-env", "github-path"} {
				data, readErr := os.ReadFile(filepath.Join(fixture, path))
				switch {
				case tc.wantFailure:
					if readErr == nil && len(data) != 0 {
						t.Errorf("failed installer published %s: %s", path, data)
					}
				case readErr != nil:
					t.Fatal(readErr)
				case path == "github-env":
					for _, key := range []string{"BEADS_TEST_BD_BINARY", "GC_INTEGRATION_REAL_BD", "GC_ACCEPTANCE_BD_BIN"} {
						if !strings.Contains(string(data), key+"="+selected+"\n") {
							t.Errorf("missing exact selection %s: %s", key, data)
						}
					}
				case string(data) != filepath.Dir(selected)+"\n":
					t.Errorf("PATH publication=%q", data)
				}
			}
			if tc.wantFailure {
				if _, err := os.Stat(selected); !os.IsNotExist(err) {
					t.Errorf("failed installer retained selected executable: %v", err)
				}
			} else {
				calls := readFile(t, fixture, "calls")
				if !strings.Contains(calls, "build -mod=readonly -trimpath -o ") || !strings.Contains(calls, " "+module+"/cmd/bd\n") || strings.Contains(calls, "./cmd/bd") {
					t.Errorf("did not build dependency package from consumer: %s", calls)
				}
				for _, required := range []string{"list -mod=mod -m -json all\n", "list -mod=mod -deps " + module + "/cmd/bd\n", "list -mod=readonly -m -json all\n"} {
					if !strings.Contains(calls, required) {
						t.Errorf("missing private graph hydration command %q: %s", required, calls)
					}
				}
				if got := readFile(t, filepath.Dir(selected), "consumer.go.mod"); got != gomod {
					t.Errorf("installed graph go.mod drifted: %s", got)
				}
				if got := readFile(t, filepath.Dir(selected), "consumer.go.sum"); !strings.Contains(got, "example.com/cli-only v1.0.0 h1:fixture-cli-sum\n") {
					t.Errorf("installed graph does not retain hydrated private CLI sums: %s", got)
				}
				provenance := readFile(t, filepath.Dir(selected), "source-provenance")
				if provenance != module+" "+version+" "+sum+" "+ref+"\n" {
					t.Errorf("provenance=%q", provenance)
				}
			}
			if matches, err := filepath.Glob(filepath.Join(fixture, "runner-temp", "gascity-qualified-bd.*")); err != nil || len(matches) != 0 {
				t.Errorf("private scratch remains: %v %v", matches, err)
			}
			for path, want := range map[string]string{"repo/go.mod": gomod, "repo/go.sum": checksums, "old/bd": "original archive executable\n"} {
				if got := readFile(t, fixture, path); got != want {
					t.Errorf("installer modified %s", path)
				}
			}
		})
	}
}

func testNativeCIPairWiring(t *testing.T) {
	root := repoRoot(t)
	for _, suite := range []string{"acceptance", "integration"} {
		build := readFile(t, root, "test/"+suite+"/BUILD.bazel")
		if !strings.Contains(build, "\"@com_github_rhar1511_beads//cmd/bd:bd\"") || strings.Contains(build, "\"@bd_bin_v1_3_1//:bd\"") {
			t.Errorf("Bazel %s must receive the linked owner module CLI, not the independent upstream archive", suite)
		}
	}
	for workflow, jobs := range map[string][]string{
		"ci.yml":             {"beads-topology-acceptance", "beads-proxied-native-acceptance", "cmd-gc-process"},
		"mac-regression.yml": {"mac-acceptance", "mac-integration-packages", "mac-integration-rest", "mac-integration-review-formulas", "mac-cmd-gc-process"},
		"nightly.yml":        {"beads-proxied-perf", "integration-sqlite-coordstore"},
		"rc-gate.yml":        {"ubuntu_acceptance_a"},
	} {
		wf := readCriticalPathWorkflow(t, workflow)
		for _, name := range jobs {
			job, ok := wf.Jobs[name]
			if !ok {
				t.Fatalf("%s missing paired job %s", workflow, name)
			}
			setups := 0
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "./.github/actions/setup-gascity-") {
					setups++
					if step.With["qualified-integration-bd"] != "true" {
						t.Errorf("%s/%s does not select authenticated native pair", workflow, name)
					}
				}
				if strings.Contains(step.Run, "BD_CURRENT_REF") || strings.Contains(step.Run, "git clone") {
					t.Errorf("%s/%s overrides the native pair with an independent upstream source build", workflow, name)
				}
			}
			if setups != 1 {
				t.Errorf("%s/%s setup count=%d, want1", workflow, name, setups)
			}
		}
	}
	mac := readCriticalPathWorkflow(t, "mac-regression.yml")
	foundArchiveContract := false
	for _, step := range mac.Jobs["mac-acceptance"].Steps {
		if step.Run == "make test-bd-cli-contract" {
			foundArchiveContract = true
			if step.Env["EXTRA_TEST_ENV"] != "GC_ACCEPTANCE_BD_BIN=${{ env.BEADS_TEST_UPSTREAM_BD_BINARY }}" {
				t.Error("Mac independent archive CLI contract lost its original selected distribution")
			}
		}
	}
	if !foundArchiveContract {
		t.Fatal("Mac independent archive CLI contract is missing")
	}
	for _, platform := range []string{"ubuntu", "macos"} {
		action := readFile(t, root, ".github/actions/setup-gascity-"+platform+"/action.yml")
		if !strings.Contains(action, "BEADS_TEST_UPSTREAM_BD_BINARY=%s") {
			t.Errorf("%s setup does not retain the separately installed archive selection", platform)
		}
	}
	ci := readCriticalPathWorkflow(t, "ci.yml")
	for _, name := range []string{"contract-acceptance-previous", "contract-acceptance-current"} {
		for _, step := range ci.Jobs[name].Steps {
			if step.With["qualified-integration-bd"] == "true" || strings.Contains(step.Run, "install-qualified-integration-bd") {
				t.Errorf("native pair replaced independent upstream compatibility cell %s", name)
			}
		}
	}
}

// testNativeMailFixtureInputCapture keeps one selected test executable path
// across both canonical scrubs without preserving live BEADS credentials.
func testNativeMailFixtureInputCapture(t *testing.T) {
	path := filepath.Join(repoRoot(t), "cmd/gc/cmd_mail_test.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	const input = "selectedNativeMailFixtureInput"
	captures := 0
	for _, decl := range file.Decls {
		decl, ok := decl.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			continue
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != input || len(value.Values) != 1 {
				continue
			}
			call, ok := value.Values[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				t.Fatal("native mail fixture input must be captured once at package initialization")
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Getenv" {
				t.Fatal("native mail fixture input must read only its selected test key")
			}
			owner, ok := selector.X.(*ast.Ident)
			argument, stringArg := call.Args[0].(*ast.BasicLit)
			if !ok || owner.Name != "os" || !stringArg || argument.Value != `"BEADS_TEST_BD_BINARY"` {
				t.Fatal("native mail fixture input must capture only BEADS_TEST_BD_BINARY, never backend credentials")
			}
			captures++
		}
	}
	if captures != 1 {
		t.Fatalf("native mail fixture package-init input captures=%d, want1", captures)
	}
	var gate, validation token.Pos
	bound := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if assign, ok := node.(*ast.AssignStmt); ok {
				for _, left := range assign.Lhs {
					if name, ok := left.(*ast.Ident); ok && name.Name == input {
						t.Error("captured native mail fixture input must never be updated by a test")
					}
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fn.Name.Name == "selectedNativeMailFixtureBD" {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Getenv" {
					t.Error("native mail validation must take captured input, not read the scrubbed environment")
				}
			}
			if fn.Name.Name != "TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox" {
				return true
			}
			callee, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch callee.Name {
			case "startPasswordedDoltServer":
				gate = call.Pos()
			case "selectedNativeMailFixtureBD":
				validation = call.Pos()
				if len(call.Args) == 2 {
					argument, ok := call.Args[1].(*ast.Ident)
					bound = ok && argument.Name == input
				}
			}
			return true
		})
	}
	if !bound || gate == token.NoPos || validation <= gate {
		t.Fatal("native mail fixture must validate the captured selected path after its existing real-Dolt slow gate")
	}
}

func testNativeRigWorktreeFixtureSelection(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot(t), "cmd/gc/cmd_bd_test.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fixture *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore" {
			fixture = fn
		}
	}
	if fixture == nil {
		t.Fatal("rig worktree fixture is missing")
	}
	var gate, validation token.Pos
	bound, pinnedConfig := false, false
	ast.Inspect(fixture.Body, func(node ast.Node) bool {
		if value, ok := node.(*ast.BasicLit); ok && value.Kind == token.STRING && strings.Contains(value.Value, "[workspace.env]") && strings.Contains(value.Value, "BD_BIN = ") {
			pinnedConfig = true
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch callee.Name {
		case "waitTestRealBDPath", "buildPinnedBDBinaryForTests":
			t.Error("native rig fixture must not substitute the pure-Go compatibility builder")
		case "startPasswordedDoltServer":
			gate = call.Pos()
		case "selectedNativeMailFixtureBD":
			validation = call.Pos()
			if len(call.Args) == 2 {
				argument, ok := call.Args[1].(*ast.Ident)
				bound = ok && argument.Name == "selectedNativeMailFixtureInput"
			}
		}
		return true
	})
	if !bound || gate == token.NoPos || validation <= gate {
		t.Error("native rig fixture must validate the genuine captured executable after its existing real-Dolt gate")
	}
	if !pinnedConfig {
		t.Error("native rig fixture must bind workspace.env.BD_BIN for gc/provider preflight as well as raw bd")
	}
}

func testNativeBazelIntegrationSelection(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot(t), "test/integration/integration_test.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var main, selector *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			switch fn.Name.Name {
			case "TestMain":
				main = fn
			case "bazelIntegrationBDBinary":
				selector = fn
			}
		}
	}
	if main == nil || selector == nil {
		t.Fatal("Bazel integration must own an explicit fail-closed owner-module selector")
	}
	guarded, missingFails := false, false
	ast.Inspect(main.Body, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok || branch.Init == nil {
			return true
		}
		assignment, ok := branch.Init.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			return true
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		callee, ok := call.Fun.(*ast.Ident)
		argument, named := call.Args[0].(*ast.Ident)
		if !ok || callee.Name != "binaryOverride" || !named || argument.Name != "integrationRealBDBinaryEnv" {
			return true
		}
		// Keep the valid explicit override branch before Bazel resolution; the
		// non-Bazel go-install branch must remain outside the Bazel branch.
		override, ok := branch.Else.(*ast.IfStmt)
		if !ok {
			return true
		}
		bazel, ok := override.Else.(*ast.IfStmt)
		if !ok {
			return true
		}
		condition, ok := bazel.Cond.(*ast.CallExpr)
		if !ok {
			return true
		}
		predicate, ok := condition.Fun.(*ast.SelectorExpr)
		if !ok || predicate.Sel.Name != "IsBazel" {
			return true
		}
		ast.Inspect(bazel.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "bazelIntegrationBDBinary" {
					guarded = true
				}
			}
			return true
		})
		return true
	})
	ast.Inspect(selector.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				if callee.Name == "panic" && len(call.Args) == 1 {
					if text, ok := call.Args[0].(*ast.BasicLit); ok && strings.Contains(text.Value, "@com_github_rhar1511_beads//cmd/bd:bd") {
						missingFails = true
					}
				}
				if callee.Name == "buildPinnedIntegrationBDBinary" || callee.Name == "runfilesBinaryAt" {
					t.Error("Bazel selector must not fall back to builds or archive data")
				}
			case *ast.SelectorExpr:
				if callee.Sel.Name == "LookPath" || callee.Sel.Name == "Command" || callee.Sel.Name == "CommandContext" {
					t.Error("Bazel selector must not fall back to ambient PATH or processes")
				}
			}
		}
		return true
	})
	if !guarded || !missingFails {
		t.Error("Bazel owner selection must follow the explicit override and fail explicitly when its declared runfile is missing")
	}
}
