package scripts_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContainerCLIToolsRebuildWithPatchedGRPC(t *testing.T) {
	const (
		ghVersion                 = "2.96.0"
		ghSourceRef               = "b300f2ec7ec9dc9addc39b2ad88c54097ded7ca0"
		doltSourceRef             = "781cbb730221ea7df4fc7995255bb336df9c3864"
		grpcVersion               = "1.83.2"
		ghSourceSHA256            = "a0c18c98c73f7333f73e19b3a0bf5bd18673f3dc226193ab6478b3ea1ea18f03"
		doltSourceSHA256          = "0b0c9bce8baef26baa7e0e5825cd2d7d6101daf6fc9673f38dac9670afb66847"
		doltToolchainRelease      = "20260611_0.0.5_trixie"
		doltOptcrossX8664SHA256   = "caf703fb1cbc0c9ff9a5b506f73da6c6f5233c04a455e638cdc50267a4d0c0c0"
		doltOptcrossAarch64SHA256 = "5635d0b38343fefb0c2b600d61c49ad9ceeaa1107bccdec8a60b1789100dc0ce"
		doltICUStaticSHA256       = "8b0234f16da73b9c8d47f86eeef98928879611149e3ee1bb560dddb0ffdd95a1"
	)

	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.base")
	for _, want := range []string{
		"ARG GH_VERSION=" + ghVersion,
		"ARG GH_SOURCE_REF=" + ghSourceRef,
		"ARG GH_SOURCE_SHA256=" + ghSourceSHA256,
		"ARG DOLT_SOURCE_REF=" + doltSourceRef,
		"ARG DOLT_SOURCE_SHA256=" + doltSourceSHA256,
		"ARG GRPC_VERSION=" + grpcVersion,
		"ARG DOLT_TOOLCHAIN_RELEASE=" + doltToolchainRelease,
		"ARG DOLT_OPTCROSS_X86_64_SHA256=" + doltOptcrossX8664SHA256,
		"ARG DOLT_OPTCROSS_AARCH64_SHA256=" + doltOptcrossAarch64SHA256,
		"ARG DOLT_ICU_STATIC_SHA256=" + doltICUStaticSHA256,
		`grep -Fq "Version = \"${DOLT_VERSION}\"" cmd/dolt/doltversion/version.go`,
		`CGO_LDFLAGS="-static -s"`,
		`-tags="icu_static,timetzdata"`,
		"x86_64-linux-musl-gcc",
		"aarch64-linux-musl-gcc",
		`file /out/dolt | grep -Fq "statically linked"`,
		"COPY --from=tool-builder /out/gh /usr/bin/gh",
		"COPY --from=tool-builder /out/dolt /usr/local/bin/dolt",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("contrib/k8s/Dockerfile.base missing %q", want)
		}
	}
	if got := strings.Count(dockerfile, `go get "google.golang.org/grpc@v${GRPC_VERSION}"`); got != 2 {
		t.Errorf("contrib/k8s/Dockerfile.base applies the grpc override %d times, want exactly 2 (gh and Dolt)", got)
	}

	for _, forbidden := range []string{
		"apt-get install -y --no-install-recommends gh",
		`/tmp/install-dolt-archive.sh "${DOLT_VERSION}"`,
		"libicu74",
		"-tags=timetzdata",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("contrib/k8s/Dockerfile.base still installs vulnerable prebuilt tool via %q", forbidden)
		}
	}
}

func TestAgentImageRebuildsBDAndGCWithPatchedGRPC(t *testing.T) {
	const (
		bdSourceRef    = "696e3967be5e1f43a5fcacb80f79c878d85d2196"
		bdSourceSHA256 = "414e59e2d7fe6a729eb95419ce58abfee9913ada92763bdd95bb7b1006256b21"
		bdBuild        = "696e3967be5"
		bdBranch       = "HEAD"
		grpcVersion    = "1.83.2"
		thriftVersion  = "0.24.0"
	)

	root := repoRoot(t)
	env := readDotenv(t, root+"/deps.env")
	// The image stamps -X main.Version=${BD_VERSION} onto source fetched at
	// BD_SOURCE_REF, and the Dockerfile's own `grep Version = "${bd_version}"
	// cmd/bd/version.go` fails the build if those two name different releases.
	// Assert it here rather than discovering it in a docker build CI may not run.
	//
	// The anchor is BD_CURRENT_VERSION, not BD_VERSION: BD_SOURCE_REF tracks
	// BD_CURRENT_REF (TestBDVersionPins), so the version that source declares is
	// BD_CURRENT_VERSION. deps.env BD_VERSION is a different role -- the
	// published tarball CI installs -- and it legitimately lags whenever the
	// current cell is pinned to a commit upstream never cut a release for, which
	// is the normal state of a bleeding-edge cell. Tying this to BD_VERSION
	// would forbid that lag and collapse two anchors the matrix keeps distinct.
	bdVersion := env["BD_CURRENT_VERSION"]
	if bdVersion == "" {
		t.Fatal("deps.env missing BD_CURRENT_VERSION")
	}

	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	for _, want := range []string{
		"ARG BD_VERSION=" + bdVersion,
		"ARG BD_SOURCE_REF=" + bdSourceRef,
		"ARG BD_SOURCE_SHA256=" + bdSourceSHA256,
		"ARG BD_BUILD=" + bdBuild,
		"ARG BD_BRANCH=" + bdBranch,
		"ARG GRPC_VERSION=" + grpcVersion,
		"ARG THRIFT_VERSION=" + thriftVersion,
		`https://github.com/gastownhall/beads/archive/${BD_SOURCE_REF}.tar.gz`,
		`echo "${BD_SOURCE_SHA256}  /tmp/bd-source.tar.gz" | sha256sum --check --strict`,
		`grep -Fq "Version = \"${bd_version}\"" cmd/bd/version.go`,
		`go get "google.golang.org/grpc@v${GRPC_VERSION}"`,
		`go get "github.com/apache/thrift@v${THRIFT_VERSION}"`,
		`go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep github.com/apache/thrift v${THRIFT_VERSION} "`,
		`CGO_ENABLED=1 go build`,
		`-tags="gms_pure_go"`,
		`-X main.Version=${bd_version}`,
		`-X main.Build=${BD_BUILD}`,
		`-X main.Commit=${BD_SOURCE_REF}`,
		`-X main.Branch=${BD_BRANCH}`,
		`COPY --from=bd-builder /out/bd /usr/local/bin/bd`,
		`CGO_ENABLED=0 go build -o gc ./cmd/gc`,
		`RUN gc version`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("contrib/k8s/Dockerfile.agent missing %q", want)
		}
	}
	if got := strings.Count(dockerfile, `go get "google.golang.org/grpc@v${GRPC_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.agent applies the bd grpc override %d times, want exactly 1", got)
	}
	if got := strings.Count(dockerfile, `go get "github.com/apache/thrift@v${THRIFT_VERSION}"`); got != 1 {
		t.Errorf("contrib/k8s/Dockerfile.agent applies the bd thrift override %d times, want exactly 1", got)
	}
	if strings.Contains(dockerfile, "COPY bd /usr/local/bin/bd") {
		t.Error("contrib/k8s/Dockerfile.agent still copies the vulnerable prebuilt bd binary")
	}
	baseImageArg := strings.Index(dockerfile, "ARG BASE_IMAGE=")
	firstStage := strings.Index(dockerfile, "FROM ")
	if baseImageArg < 0 || firstStage < 0 || baseImageArg > firstStage {
		t.Error("contrib/k8s/Dockerfile.agent must declare BASE_IMAGE globally before its first FROM")
	}

	goMod := readFile(t, root, "go.mod")
	wantGRPCModule := "google.golang.org/grpc v" + grpcVersion
	if got := strings.Count(goMod, wantGRPCModule); got != 1 {
		t.Errorf("go.mod contains %q %d times, want exactly 1 so the gc binary embeds the patched grpc", wantGRPCModule, got)
	}

	workflow := readFile(t, root, ".github/workflows/container-scan.yml")
	if !strings.Contains(workflow, "CGO_ENABLED=0 go build -o gc ./cmd/gc") {
		t.Error("container scan must build gc with the release's portable CGO_ENABLED=0 configuration")
	}
}

// TestMCPMailImagePinsPatchedPythonDependencies checks the normalized package
// version contracts consumed by the mail image's hash-locked pip installation.
// Newer patched versions are valid; comments cannot satisfy a security floor.
func TestMCPMailImagePinsPatchedPythonDependencies(t *testing.T) {
	root := repoRoot(t)
	input := readFile(t, root, ".github/requirements/mcp-agent-mail.in")
	overrides := readFile(t, root, ".github/requirements/mcp-agent-mail.overrides.txt")
	lock := readFile(t, root, ".github/requirements/mcp-agent-mail.txt")
	for _, floor := range []struct {
		name, version, lockVersion string
		override                   bool
	}{
		{"fsspec", "2026.6.0", "2026.6.0", false},
		{"gitpython", "3.1.60", "3.2.0", false},
		{"aiohttp", "3.14.3", "3.14.3", false},
		{"anyio", "4.14.2", "4.14.2", false},
		{"pillow", "12.3.0", "12.3.0", false},
		{"urllib3", "2.8.0", "2.8.0", false},
		{"pyjwt", "2.14.0", "2.15.1", true},
		{"cryptography", "50.0.0", "50.0.0", true},
	} {
		t.Run(floor.name, func(t *testing.T) {
			declaration := input
			if floor.override {
				declaration = overrides
			}
			for _, contract := range []struct{ text, operator, version string }{
				{declaration, ">=", floor.version}, {lock, "==", floor.lockVersion},
			} {
				want := parseModuleSemver(t, "v"+contract.version)
				have := mailRequirementVersion(t, contract.text, floor.name, contract.operator)
				if !semverAtLeast(have, want) {
					t.Errorf("%s %s contract resolves below patched version %s", floor.name, contract.operator, contract.version)
				}
			}
		})
	}
}

// mailRequirementVersion reads the supported numeric version contract from a
// pip requirements declaration, ignoring hashes, continuations and comments.
func mailRequirementVersion(t *testing.T, requirements, name, operator string) [3]int {
	t.Helper()
	var version [3]int
	found := false
	for _, raw := range strings.Split(requirements, "\n") {
		line, _, _ := strings.Cut(raw, "#")
		pkg, value, ok := strings.Cut(strings.TrimSpace(line), operator)
		if !ok || strings.ToLower(strings.TrimSpace(pkg)) != name {
			continue
		}
		if found {
			t.Fatalf("duplicate %s %s contract", name, operator)
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			t.Fatalf("missing %s version", name)
		}
		version = parseModuleSemver(t, "v"+fields[0])
		found = true
	}
	if !found {
		t.Fatalf("missing %s %s package contract", name, operator)
	}
	return version
}

// TestMCPMailImageUpgradesPatchedOSPackages guards the --only-upgrade list in
// Dockerfile.mail. The base image is pinned by digest, so an OS-package CVE is only
// cleared by naming the package here, and Trivy reports every binary package of a
// source separately: dropping one name leaves that package on the vulnerable version
// and the scan red, with the other eight looking like the whole fix.
func TestMCPMailImageUpgradesPatchedOSPackages(t *testing.T) {
	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.mail")

	upgrade, _, ok := strings.Cut(dockerfile, "&& apt-get install -y --no-install-recommends \\")
	if !ok {
		t.Fatal("contrib/k8s/Dockerfile.mail has no plain apt-get install stanza to bound the --only-upgrade list")
	}
	if !strings.Contains(upgrade, "--only-upgrade") {
		t.Fatal("contrib/k8s/Dockerfile.mail no longer upgrades any pinned-base OS package")
	}

	for _, pkg := range []string{
		// openssl / systemd set, already present.
		"libcap2", "libssl3t64", "libsystemd0", "libudev1", "openssl", "openssl-provider-legacy",
		// util-linux set, CVE-2026-53615, fixed in 2.41.5-0+deb13u1.
		"bsdutils", "libblkid1", "liblastlog2-2", "libmount1", "libsmartcols1",
		"libuuid1", "login", "mount", "util-linux",
		// gzip CVE-2026-41992; libpcre2-8-0 CVE-2026-86145 and CVE-2026-89161;
		// libsqlite3-0 CVE-2026-11822 and CVE-2026-11824.
		"gzip", "libpcre2-8-0", "libsqlite3-0",
	} {
		if !strings.Contains(upgrade, "\n    "+pkg+" \\") {
			t.Errorf("contrib/k8s/Dockerfile.mail --only-upgrade list missing %q", pkg)
		}
	}
}

// TestRebuiltToolsAssertPatchedGRPCArtifact guards the artifact-level proof that
// each rebuilt CLI actually embeds the patched grpc module. Text-level ARG/recipe
// checks confirm the build inputs; these `go version -m` assertions are the only
// evidence the produced binary links grpc v${GRPC_VERSION}, so they must not be
// silently removable. bd already had one; gh and dolt now mirror it.
func TestRebuiltToolsAssertPatchedGRPCArtifact(t *testing.T) {
	root := repoRoot(t)

	base := readFile(t, root, "contrib/k8s/Dockerfile.base")
	for _, bin := range []string{"/out/gh", "/out/dolt"} {
		want := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "`
		if !strings.Contains(base, want) {
			t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched grpc; missing %q", bin, want)
		}
	}

	agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	want := `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep google.golang.org/grpc v${GRPC_VERSION} "`
	if !strings.Contains(agent, want) {
		t.Errorf("contrib/k8s/Dockerfile.agent must assert /out/bd embeds patched grpc; missing %q", want)
	}
}

// TestRebuiltToolsForcePatchedXModules guards the module overrides that replaced the
// gh and Dolt waivers in .trivyignore.yaml. The pinned gh and Dolt sources select
// x/crypto, x/net, x/text and thrift versions Trivy flags, and the grpc-only override
// left them there, which is what kept those paths waived. Dropping either the `go get`
// or its `go version -m` proof would put the vulnerable module back with nothing
// failing, so both halves are asserted here.
func TestRebuiltToolsForcePatchedXModules(t *testing.T) {
	root := repoRoot(t)
	base := readFile(t, root, "contrib/k8s/Dockerfile.base")

	// Versions each override forces, at or above what Trivy names as fixed for the findings it clears.
	for _, arg := range []string{
		"ARG XCRYPTO_VERSION=0.57.0",
		"ARG XNET_VERSION=0.60.0",
		"ARG XTEXT_VERSION=0.42.0",
		"ARG XMOD_VERSION=0.41.0",
		"ARG THRIFT_VERSION=0.24.0",
	} {
		if !strings.Contains(base, arg) {
			t.Errorf("contrib/k8s/Dockerfile.base missing %q", arg)
		}
	}

	// All gh x-module overrides must survive the final module graph selection.
	// Dolt takes no x/mod override because no x/mod package is linked into its binary.
	ghModules := map[string]string{
		"golang.org/x/crypto": "XCRYPTO_VERSION",
		"golang.org/x/net":    "XNET_VERSION",
		"golang.org/x/text":   "XTEXT_VERSION",
		"golang.org/x/mod":    "XMOD_VERSION",
	}
	doltModules := map[string]string{
		"golang.org/x/crypto":      "XCRYPTO_VERSION",
		"golang.org/x/net":         "XNET_VERSION",
		"golang.org/x/text":        "XTEXT_VERSION",
		"github.com/apache/thrift": "THRIFT_VERSION",
	}
	// Each `go get` is looked for inside its own stanza. gh and Dolt share the x/text
	// override verbatim, so a file-wide search lets one stand in for the other and a
	// dropped override reads as present here, failing only in the image build.
	ghStart := strings.Index(base, "WORKDIR /src/gh")
	doltStart := strings.Index(base, "WORKDIR /src/dolt")
	if ghStart < 0 || doltStart <= ghStart {
		t.Fatal("contrib/k8s/Dockerfile.base has no WORKDIR /src/gh stanza ahead of the Dolt one")
	}
	ghStanza := base[ghStart:doltStart]
	doltStanza := base[doltStart:]
	if next := strings.Index(doltStanza, "\nFROM "); next > 0 {
		doltStanza = doltStanza[:next]
	}
	stanzas := map[string]string{"/out/gh": ghStanza, "/out/dolt": doltStanza}

	for bin, modules := range map[string]map[string]string{"/out/gh": ghModules, "/out/dolt": doltModules} {
		for module, arg := range modules {
			get := `"` + module + `@v${` + arg + `}"`
			if !strings.Contains(stanzas[bin], get) {
				t.Errorf("contrib/k8s/Dockerfile.base must override %s inside the %s build stanza; missing %q", module, bin, get)
			}
			assert := `go version -m ` + bin + ` | tr '\t' ' ' | grep -Fq "dep ` + module + ` v${` + arg + `} "`
			if !strings.Contains(base, assert) {
				t.Errorf("contrib/k8s/Dockerfile.base must assert %s embeds patched %s; missing %q", bin, module, assert)
			}
		}
	}

	// Inside the gh stanza the x/mod get has to run after the x/text one. A later
	// `go get` naming a version below what an earlier one dragged in is a downgrade,
	// and it takes the earlier module with it: measured against the pinned source,
	// with XTEXT_VERSION at 0.39.0 the reversed order selects x/mod v0.38.0, under
	// the fixed version. The two orders agree at the versions pinned today, so this
	// is what keeps the next bump from recreating that shape.
	xtextGet := strings.Index(ghStanza, `"golang.org/x/text@v${XTEXT_VERSION}"`)
	xmodGet := strings.Index(ghStanza, `"golang.org/x/mod@v${XMOD_VERSION}"`)
	if xtextGet < 0 || xmodGet < 0 {
		t.Fatal("gh stanza is missing the x/text or the x/mod override")
	}
	if xmodGet < xtextGet {
		t.Error("gh stanza runs the x/mod override ahead of x/text; the newest constraint goes last, so a lower x/text pin cannot downgrade x/mod out from under its assertion")
	}
}

// trivyIgnoreDoc is the shape of .trivyignore.yaml the guards below read. purls
// is decoded because Trivy honors a purl-scoped waiver in every image, path or
// no path: an entry naming only `purls: ["pkg:golang/github.com/apache/thrift"]`
// is a waiver for bd, dolt and gh at once, and a guard that iterates `paths`
// never sees it.
type trivyIgnoreDoc struct {
	Vulnerabilities []struct {
		ID    string   `yaml:"id"`
		Paths []string `yaml:"paths"`
		Purls []string `yaml:"purls"`
	} `yaml:"vulnerabilities"`
}

// reviewedTrivyIgnorePurlWaivers names the purl-scoped waivers this repo has
// reviewed, as "<id> <purl>". It is empty and is meant to stay that way: a
// module-scoped waiver is exactly the "waive instead of fix" shape the rebuilt-
// tool guard forbids, and the images force the patched modules in instead
// (Dockerfile.base's GRPC_VERSION/THRIFT_VERSION and the x/* pins). Adding an
// entry here is the deliberate edit that admits one.
var reviewedTrivyIgnorePurlWaivers = map[string]bool{}

// unreviewedTrivyIgnorePurlWaivers reports every purl-scoped waiver the file
// carries that no reviewer has admitted, as a ready-to-print message.
func unreviewedTrivyIgnorePurlWaivers(doc trivyIgnoreDoc) []string {
	var found []string
	for _, v := range doc.Vulnerabilities {
		for _, purl := range v.Purls {
			if reviewedTrivyIgnorePurlWaivers[v.ID+" "+purl] {
				continue
			}
			found = append(found, fmt.Sprintf("%s waives module %q by purl, which applies to every image including the rebuilt bd, dolt and gh; move the module forward in the build instead of waiving it", v.ID, purl))
		}
	}
	return found
}

// TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools keeps findings visible for
// every rebuilt supplier. Compiler and module assertions prove the security
// floors; a path or purl waiver must not conceal a later regression.
func TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools(t *testing.T) {
	root := repoRoot(t)
	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}
	for _, unreviewed := range unreviewedTrivyIgnorePurlWaivers(doc) {
		t.Error(unreviewed)
	}
	rebuiltPaths := map[string]bool{
		"usr/local/bin/bd":      true,
		"usr/local/bin/dolt":    true,
		"usr/bin/gh":            true,
		"usr/local/bin/kubectl": true,
	}
	for _, v := range doc.Vulnerabilities {
		for _, path := range v.Paths {
			if rebuiltPaths[path] {
				t.Errorf("%s waives rebuilt tool %q; update the supplier build instead of suppressing its findings", v.ID, path)
			}
		}
	}
}

// TestTrivyIgnoreRejectsPurlScopedWaivers exercises rejection even when the
// repository ignore file is empty.
func TestTrivyIgnoreRejectsPurlScopedWaivers(t *testing.T) {
	const waived = `vulnerabilities:
  - id: CVE-2026-43871
    purls:
      - pkg:golang/github.com/apache/thrift
    expired_at: 2026-11-07
  - id: CVE-2026-56852
    paths:
      - usr/local/bin/kubectl
    expired_at: 2026-11-07
`
	var doc trivyIgnoreDoc
	if err := yaml.Unmarshal([]byte(waived), &doc); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	found := unreviewedTrivyIgnorePurlWaivers(doc)
	if len(found) != 1 {
		t.Fatalf("unreviewedTrivyIgnorePurlWaivers() = %v, want exactly the thrift purl entry", found)
	}
	if !strings.Contains(found[0], "CVE-2026-43871") || !strings.Contains(found[0], "apache/thrift") {
		t.Fatalf("finding = %q, want it to name the CVE and the module", found[0])
	}
}

// goModVersion returns the [major, minor, patch] version go.mod pins for module,
// reading the require directive directly so the guard tests never drift from the
// tree's actual module graph. Replace directives are ignored.
func goModVersion(t *testing.T, goMod, module string) [3]int {
	t.Helper()
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "replace ") || strings.Contains(line, "=>") {
			continue
		}
		line = strings.TrimPrefix(line, "require ")
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == module && strings.HasPrefix(fields[1], "v") {
			return parseModuleSemver(t, fields[1])
		}
	}
	t.Fatalf("go.mod does not pin %s", module)
	return [3]int{}
}

// parseModuleSemver parses a "vMAJOR.MINOR.PATCH" module version into comparable parts.
func parseModuleSemver(t *testing.T, v string) [3]int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		t.Fatalf("version %q is not vMAJOR.MINOR.PATCH", v)
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("parsing %q component %q: %v", v, p, err)
		}
		out[i] = n
	}
	return out
}

// semverAtLeast reports whether have is greater than or equal to want.
func semverAtLeast(have, want [3]int) bool {
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

// TestTrivyIgnoreDropsGCModuleWaiversPastThreshold enforces that no usr/local/bin/gc
// x/net, x/crypto, or grpc CVE waiver outlives the go.mod bump that fixes it. Unlike the
// rebuilt tools (bd, dolt, gh), gc is built straight from this module, so a waiver on a
// gc path is only honest while go.mod still pins a vulnerable version. Each CVE records
// the module and the first version that fixes it (taken from the waiver's own removal
// text); once go.mod reaches that version the gc path must be dropped, or the container
// scan would stay green without proving the gc binary is clean.
func TestTrivyIgnoreDropsGCModuleWaiversPastThreshold(t *testing.T) {
	root := repoRoot(t)

	type modFix struct {
		module     string
		fixVersion string
	}
	gcModuleCVEs := map[string]modFix{
		// golang.org/x/net http2, fixed in 0.53.0.
		"CVE-2026-33814": {"golang.org/x/net", "v0.53.0"},
		// golang.org/x/net HTML/idna, fixed only in 0.55.0.
		"CVE-2026-25680": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-25681": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-27136": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-39821": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-42502": {"golang.org/x/net", "v0.55.0"},
		"CVE-2026-42506": {"golang.org/x/net", "v0.55.0"},
		// golang.org/x/crypto/ssh*, fixed in 0.52.0.
		"CVE-2026-39827": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39828": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39829": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39830": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39831": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39832": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-39835": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-42508": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-46595": {"golang.org/x/crypto", "v0.52.0"},
		"CVE-2026-46597": {"golang.org/x/crypto", "v0.52.0"},
		// google.golang.org/grpc. CVE-2026-84445 is fixed in 1.82.2 on the 1.82 line
		// and 1.83.2 on the 1.83 line go.mod is on, so 1.83.1 clears only the first.
		"CVE-2026-84304": {"google.golang.org/grpc", "v1.83.1"},
		"CVE-2026-84445": {"google.golang.org/grpc", "v1.83.2"},
	}

	goMod := readFile(t, root, "go.mod")
	have := map[string][3]int{
		"golang.org/x/net":       goModVersion(t, goMod, "golang.org/x/net"),
		"golang.org/x/crypto":    goModVersion(t, goMod, "golang.org/x/crypto"),
		"google.golang.org/grpc": goModVersion(t, goMod, "google.golang.org/grpc"),
	}

	var doc struct {
		Vulnerabilities []struct {
			ID    string   `yaml:"id"`
			Paths []string `yaml:"paths"`
		} `yaml:"vulnerabilities"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, ".trivyignore.yaml")), &doc); err != nil {
		t.Fatalf("parsing .trivyignore.yaml: %v", err)
	}

	for _, v := range doc.Vulnerabilities {
		fix, tracked := gcModuleCVEs[v.ID]
		if !tracked {
			continue
		}
		waivesGC := false
		for _, p := range v.Paths {
			if p == "usr/local/bin/gc" {
				waivesGC = true
			}
		}
		if !waivesGC {
			continue
		}
		if semverAtLeast(have[fix.module], parseModuleSemver(t, fix.fixVersion)) {
			t.Errorf("%s still waives usr/local/bin/gc but go.mod pins %s >= %s, which fixes it; drop the gc path so the container scan proves the gc binary is clean", v.ID, fix.module, fix.fixVersion)
		}
	}
}

// TestGoModPinsXModPastGCFinding guards the gc half of the same container-scan
// finding the Dockerfile overrides cover for gh. gc is built straight from this
// module rather than from pinned third-party source, so no `go get` in a Dockerfile
// can move it: the only floor is go.mod's own pin, and dropping that pin back below
// the fixed version would put the vulnerable module into usr/local/bin/gc with
// nothing in the build failing.
func TestGoModPinsXModPastGCFinding(t *testing.T) {
	// The first version Trivy names as fixed for the x/mod findings on usr/local/bin/gc.
	const xmodFixVersion = "v0.40.0"

	goMod := readFile(t, repoRoot(t), "go.mod")
	have := goModVersion(t, goMod, "golang.org/x/mod")
	if !semverAtLeast(have, parseModuleSemver(t, xmodFixVersion)) {
		t.Errorf("go.mod pins golang.org/x/mod below %s, so the gc binary carries the flagged module; raise the pin rather than waiving the gc path", xmodFixVersion)
	}
}

// TestTrivyIgnoreKeepsReviewedBridgeEntries holds main's time-boxed waiver bridge
// (#5885) retired. The bridge carried four reviewed entries on its own 2026-09-21
// horizon rather than this file's, for findings no rebuild cleared at the time; every
// one of them was closed by moving the pin its statement named, not by re-dating it.
// CVE-2026-43871 went when contrib/k8s/Dockerfile.base's THRIFT_VERSION reached
// 0.24.0 (TestRebuiltToolsForcePatchedXModules pins that), and the three GitPython
// CVEs went when .github/requirements/mcp-agent-mail.txt reached 3.1.59
// (TestMCPMailImagePinsPatchedPythonDependencies pins that).
//
// So the guard is now the absence of the bridge: no entry may carry a horizon at or
// behind the bridge's, because such an entry is either the bridge coming back under a
// new statement or a waiver Trivy already treats as expired, and an expired waiver
// fails the scan gate at the next run rather than at review time. Re-dating one of
// these forward is a deliberate decision that belongs in this file's own horizon with
// a statement to match, which is exactly the edit this test forces.
func TestTrivyIgnoreKeepsReviewedBridgeEntries(t *testing.T) {
	// An explicit empty sequence is a valid no-waiver policy. Missing or null
	// configuration must still fail instead of silently parsing as no waivers.
	// Exercise the expiry predicate independently of today's empty document.
	validate := func(text string) []string {
		var doc struct {
			Vulnerabilities []struct {
				ID        string `yaml:"id"`
				ExpiredAt string `yaml:"expired_at"`
			} `yaml:"vulnerabilities"`
		}
		if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
			return []string{fmt.Sprintf("parsing .trivyignore.yaml: %v", err)}
		}
		if doc.Vulnerabilities == nil {
			return []string{".trivyignore.yaml must declare vulnerabilities as an explicit sequence"}
		}

		// ISO-8601 dates compare correctly as strings, so <= is "at or behind".
		const bridgeHorizon = "2026-09-21"
		var issues []string
		for _, v := range doc.Vulnerabilities {
			if v.ExpiredAt == "" {
				issues = append(issues, fmt.Sprintf("%s has no expired_at; every waiver in this file is time-boxed", v.ID))
				continue
			}
			if v.ExpiredAt <= bridgeHorizon {
				issues = append(issues, fmt.Sprintf("%s expires %s, at or behind the retired bridge horizon %s; fix the finding as the bridge's own entries were, or move it to this file's horizon with a statement saying why", v.ID, v.ExpiredAt, bridgeHorizon))
			}
		}
		return issues
	}

	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{name: "explicit-empty", text: "vulnerabilities: []\n"},
		{name: "reviewed-horizon", text: "vulnerabilities:\n  - id: CVE-test\n    expired_at: \"2026-10-30\"\n"},
		{name: "missing-sequence", text: "{}\n", want: "explicit sequence"},
		{name: "null-sequence", text: "vulnerabilities: null\n", want: "explicit sequence"},
		{name: "malformed-yaml", text: "vulnerabilities: [\n", want: "parsing .trivyignore.yaml"},
		{name: "wrong-sequence-type", text: "vulnerabilities: {}\n", want: "parsing .trivyignore.yaml"},
		{name: "missing-expiry", text: "vulnerabilities:\n  - id: CVE-test\n", want: "has no expired_at"},
		{name: "at-retired-horizon", text: "vulnerabilities:\n  - id: CVE-test\n    expired_at: \"2026-09-21\"\n", want: "at or behind the retired bridge horizon"},
		{name: "before-retired-horizon", text: "vulnerabilities:\n  - id: CVE-test\n    expired_at: \"2026-09-20\"\n", want: "at or behind the retired bridge horizon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := validate(tc.text)
			if tc.want == "" {
				if len(issues) != 0 {
					t.Fatalf("valid policy rejected: %s", strings.Join(issues, "\n"))
				}
				return
			}
			if !strings.Contains(strings.Join(issues, "\n"), tc.want) {
				t.Fatalf("invalid policy yielded %v, want %q", issues, tc.want)
			}
		})
	}

	for _, issue := range validate(readFile(t, repoRoot(t), ".trivyignore.yaml")) {
		t.Error(issue)
	}
}

// Rebuilt suppliers must use the patched compiler as well as patched modules:
// changing this repository's go.mod does not change their embedded Go stdlib.
func TestContainerSupplierToolchainsAndBDXModules(t *testing.T) {
	root := repoRoot(t)
	const toolchain = "golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c"
	for path, binaries := range map[string][]string{
		"contrib/k8s/Dockerfile.base":       {"gh", "dolt"},
		"contrib/k8s/Dockerfile.agent":      {"bd"},
		"contrib/k8s/Dockerfile.controller": {"kubectl"},
	} {
		dockerfile := readFile(t, root, path)
		if !strings.Contains(dockerfile, "FROM "+toolchain+" AS ") || !strings.Contains(dockerfile, "GOTOOLCHAIN=local") {
			t.Errorf("%s must bind its supplier builder to the immutable patched compiler without automatic toolchain switching", path)
		}
		for _, binary := range binaries {
			want := `go version -m /out/` + binary + ` | grep -Fq "/out/` + binary + `: go1.26.9"`
			if !strings.Contains(dockerfile, want) {
				t.Errorf("%s must assert %s embeds patched Go; missing %q", path, binary, want)
			}
		}
	}
	agent := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	for module, arg := range map[string]string{"golang.org/x/net": "XNET_VERSION", "golang.org/x/crypto": "XCRYPTO_VERSION", "golang.org/x/text": "XTEXT_VERSION"} {
		if !strings.Contains(agent, `"`+module+`@v${`+arg+`}"`) || !strings.Contains(agent, `go version -m /out/bd | tr '\t' ' ' | grep -Fq "dep `+module+` v${`+arg+`} "`) {
			t.Errorf("bd must both select and assert patched %s", module)
		}
	}
	for _, want := range []string{"ARG XNET_VERSION=0.60.0", "ARG XCRYPTO_VERSION=0.57.0", "ARG XTEXT_VERSION=0.42.0"} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent builder missing %q", want)
		}
	}
}

// Kubernetes' checked-in vendor/workspace graph must not bypass the patched
// dependencies, and the rebuilt client must retain its official source identity.
func TestControllerRebuildsSameKubectlRelease(t *testing.T) {
	dockerfile := readFile(t, repoRoot(t), "contrib/k8s/Dockerfile.controller")
	for _, want := range []string{
		"ARG KUBECTL_VERSION=v1.36.3",
		"ARG KUBECTL_SOURCE_REF=0f29094e5b73085e3802ecc1298ecae13866bfe6",
		"ARG KUBECTL_SOURCE_SHA256=8877f821fe517fa5d00df5f88ce416f47ef279b85f5aba6eb1fa4a0da7511e72",
		"ARG TARGETARCH", "GOWORK=off", "GOFLAGS=-mod=mod",
		"ARG XNET_VERSION=0.60.0", "ARG XCRYPTO_VERSION=0.57.0", "ARG XTEXT_VERSION=0.42.0",
		`https://github.com/kubernetes/kubernetes/archive/${KUBECTL_SOURCE_REF}.tar.gz`,
		`echo "${KUBECTL_SOURCE_SHA256}  /tmp/kubectl-source.tar.gz" | sha256sum --check --strict`,
		`CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" go build`,
		`-X k8s.io/component-base/version.gitVersion=${KUBECTL_VERSION}`,
		`-X k8s.io/component-base/version.gitCommit=${KUBECTL_SOURCE_REF}`,
		`./cmd/kubectl`, `COPY --from=kubectl-builder /out/kubectl /usr/local/bin/kubectl`,
		`/out/kubectl version --client --output=json`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("controller builder missing %q", want)
		}
	}
	for module, arg := range map[string]string{"golang.org/x/net": "XNET_VERSION", "golang.org/x/crypto": "XCRYPTO_VERSION", "golang.org/x/text": "XTEXT_VERSION"} {
		if !strings.Contains(dockerfile, `"`+module+`@v${`+arg+`}"`) || !strings.Contains(dockerfile, `go version -m /out/kubectl | tr '\t' ' ' | grep -Fq "dep `+module+` v${`+arg+`} "`) {
			t.Errorf("kubectl must both select and assert patched %s", module)
		}
	}
	if strings.Contains(dockerfile, "https://dl.k8s.io/release/") {
		t.Error("controller still installs a vulnerable prebuilt kubectl")
	}
}
