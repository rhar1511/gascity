package scripts_test

import (
	"fmt"
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
