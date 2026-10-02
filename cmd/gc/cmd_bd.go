package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionauthority"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/spf13/cobra"
)

// bdSilentFallbackExitCode is the exit code gc bd emits when it detects
// that bd silently fell back to on-disk auto-import mode (managed Dolt
// unreachable). Distinct from bd's own exits so operators and CI can
// tell the loud-fail apart from a real bd error. Covers both the
// bd update path (gastownhall/gascity#2080) and the bd close path
// (gastownhall/gascity#2079) because both subcommands flow through doBd.
const bdSilentFallbackExitCode = 4

const bdSilentFallbackUserMessage = "gc bd: managed Dolt unreachable; bd fell back to on-disk auto-import mode. If this command wrote data, that write was NOT persisted. Restart the managed Dolt server (or check connectivity) and retry. (See gastownhall/gascity#2080.)"

// bdDoltStartConflictUserMessage is appended (bd's own output is left
// intact) when bd's error output suggests running `bd dolt start` to
// recover from an unreachable managed Dolt server. That command starts a
// second, unmanaged Dolt server that conflicts with gc's own managed server
// on the same data directory, so gc bd points at the gc-managed remedy
// instead. See gastownhall/gascity#1374.
const bdDoltStartConflictUserMessage = "gc bd: bd suggested \"bd dolt start\" to recover, but that starts a second, unmanaged Dolt server that will conflict with gc's managed server on the same data directory. Run \"gc start\" (or \"gc dolt restart\") to restart the managed Dolt server instead, then retry. (See gastownhall/gascity#1374.)"

// bdStderrScanLimit caps how much of bd's stderr gc retains to scan for the
// silent-fallback marker. bd emits the marker pair while opening the store —
// before it runs the subcommand — so the marker, when present, always lands
// within the first chunk of stderr. Capping the retained prefix keeps memory
// bounded for bd subcommands that stream large stderr output.
const bdStderrScanLimit = 64 << 10 // 64 KiB

// headLimitedWriter retains only the first limit bytes written to it and
// discards the rest, so scanning bd's stderr for the silent-fallback marker
// never holds an unbounded copy of the stream. It always reports a full
// write so it is safe as an io.MultiWriter sink.
type headLimitedWriter struct {
	buf   []byte
	limit int
}

func (w *headLimitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return len(p), nil
}

func (w *headLimitedWriter) String() string { return string(w.buf) }

func newBdCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bd [bd-args...]",
		Short: "Run bd in the correct rig directory",
		Long: `Run a bd command routed to the correct rig directory.

When beads belong to a rig (not the city root), bd must run from the
rig directory to find the correct .beads database. This command resolves
the rig automatically from the --rig flag or by detecting the bead prefix
in the arguments.

Use --rig <name> to pin a specific rig store, or --city <path> to pin the
city (HQ) store. An explicit --city is a true scope override: it forces the
city store and disables rig auto-detection (GC_RIG, cwd, bead prefix), so a
deliberate city-scoped query is never silently downgraded to a rig store.

On a city that serves a coordination class from its own [storage] binding,
a by-id read or write of a bead that binding owns is answered in process
from the binding, not by bd against a work store that does not hold it.
--rig is refused for those beads rather than ignored or honored: it names a
work scope, and a relocated class is not partitioned by rig, so there is
nothing to narrow within. Drop --rig for a class-owned id. Auto-detected
scope (GC_RIG, -C, cwd) is unaffected, and --city still selects which city's
binding answers.

"gc bd ready" is refused outright on such a city, whatever arguments it is
given, and so is "gc bd list --ready", which bd documents as the same
semantics: both compute a frontier over one ledger and take no selector that
could reach another, so the answer is the work-class subset of the city's
ready set with no way to tell. Use "gc ready", which federates every store
the city spreads work across. It is flag-compatible with the "bd ready"
invocation the generated work query builds, not with all of "bd ready" —
"gc ready --help" lists what it takes. A city that relocates no class is
unaffected.

Generic bead output is projected before presentation: private signed-answer
and attempt-evidence records appear as private stubs, and private metadata is
removed from public owners. Read commands request complete JSON from bd; gc
renders text unless --json is requested. Provider diagnostics are withheld on
these paths. Raw SQL, exports, history, and output-field selection are refused
because they cannot preserve the private-record boundary. Scoped private reads
retain their separate authorization requirements.

A "list" that filters on the wisps (ephemeral) tier —
"--type=molecule", "--type=wisp", "--mol-type", "--wisp-type" — also gets
"--include-infra". bd skips that tier on any list without the flag, so those
filters would otherwise return [] and exit 0 on a ledger full of live
molecules. Query filters retain their meaning. "heartbeat
<issue-id>" forwards to bd's native heartbeat, which refreshes the claim's
lease and fails loudly when the caller no longer owns it. "show <id>
--watch" (or "show --current --watch", or the "view" alias) on a scope that
uses bd's proxied-server transport (the default for a new city), where bd
refuses watch mode, is served by gc instead: it re-runs "bd show" every 2
seconds and redraws when the bead's status or update time changes, until
Ctrl+C. Like bd's own watch, it renders the plain form and ignores show's
display flags (--json, --short, --long, --refs, --children). gc adds one
subcommand of its own: "release-if-current <issue-id> <assignee>", which
conditionally resets an in-progress assignment only when the bead still has
that assignee.

gc bd forces BD_EXPORT_AUTO=false to prevent bd's git auto-export hook
from wedging the wrapper after printing command output. If you need
auto-export behavior, invoke bd directly.`,
		Example: `  gc bd --rig my-project list
  gc bd --rig my-project create "New task"
  gc bd show my-project-abc          # auto-detects rig from bead prefix
  gc bd list --rig my-project -s open
  gc bd --city /path/to/city list    # pins the city (HQ) store, no rig auto-detect
  gc bd heartbeat my-project-abc     # refresh the claim lease you hold
  gc bd release-if-current my-project-abc worker-1`,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Plumb doBd's numeric exit code through exitForCode so the
			// process exit code matches the documented contract above
			// (bdSilentFallbackExitCode = 4) and bd's own exit codes are
			// preserved. Returning errExit on any non-zero would collapse
			// every code to 1 and defeat the operator/CI signal the loud-
			// fail was meant to provide.
			return exitForCode(doBd(args, stdout, stderr))
		},
	}
	return cmd
}

// bdBeadExists reports whether a bead ID resolves in a candidate store. It is
// called only to decide which store a bd invocation is scoped to, so it takes
// the city config the caller already loaded: without it, every candidate probe
// re-loaded the whole city config inside the store open.
var bdBeadExists = func(cityPath string, cfg *config.City, target execStoreTarget, beadID string) bool {
	store, err := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
	if err != nil {
		return false
	}
	bead, err := store.Get(beadID)
	return err == nil && strings.TrimSpace(bead.ID) != ""
}

func bdCommandEnv(cityPath string, cfg *config.City, target execStoreTarget) ([]string, error) {
	var overrides map[string]string
	var err error
	if target.ScopeKind == "rig" {
		overrides, err = bdRuntimeEnvForRigWithError(cityPath, cfg, target.ScopeRoot)
	} else {
		overrides, err = bdRuntimeEnvWithError(cityPath)
	}
	if err != nil {
		return nil, err
	}
	if target.ScopeKind != "rig" {
		overrides["GC_RIG"] = ""
		overrides["GC_RIG_ROOT"] = ""
		overrides["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	overrides["GC_STORE_ROOT"] = target.ScopeRoot
	overrides["GC_STORE_SCOPE"] = target.ScopeKind
	overrides["GC_BEADS_PREFIX"] = target.Prefix
	applyExportSuppressionEnv(overrides)
	// bd may invoke gc again through its provider/lifecycle hooks. Pin those
	// recursive calls to this exact executable rather than inheriting an
	// ambient GC_BIN or resolving an unrelated gc from PATH.
	executable, err := resolveBdInvokingGCBinary()
	if err != nil {
		return nil, err
	}
	overrides["GC_BIN"] = executable
	return mergeRuntimeEnv(os.Environ(), overrides), nil
}

func resolveBdInvokingGCBinary() (string, error) {
	executable, err := resolveInvokingExecutable()
	if err != nil {
		return "", fmt.Errorf("resolve invoking gc executable: %w", err)
	}
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("invoking gc executable %q is not absolute", executable)
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("canonicalize invoking gc executable: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat invoking gc executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("invoking gc executable %q is not an executable regular file", canonical)
	}
	return canonical, nil
}

// pinBdGCEnvironment replaces ambient GC_BIN with the physical invoking
// executable before the beads runner merges the child environment.
func pinBdGCEnvironment(env map[string]string) error {
	gcBin, err := resolveBdInvokingGCBinary()
	if err != nil {
		return err
	}
	env["GC_BIN"] = gcBin
	return nil
}

// bestEffortBdGCBinaryWarning fires once per process.
var bestEffortBdGCBinaryWarning sync.Once

// pinBdGCEnvironmentBestEffort is pinBdGCEnvironment for the legacy managed
// path, where main never set GC_BIN for gc's own bd invocations at all.
//
// The strict resolver refuses when the physical path behind os.Executable() is
// gone — a Homebrew/Nix upgrade that removed the old store or Cellar directory a
// still-running supervisor resolved through. Making that refusal fatal for every
// bd runner turned an upgrade into a supervisor whose session reconciler, store
// opens, health, recover and its own SIGTERM shutdown all fail until restart,
// leaving the managed Dolt unstopped. Where GC_BIN is load-bearing — the bd
// store bridge's hook callbacks, and provider-owned scopes where bd re-invokes
// gc — the refusal stays; here the pin degrades to main's own fallback chain and
// says so once.
func pinBdGCEnvironmentBestEffort(env map[string]string) {
	if env == nil {
		return
	}
	gcBin, err := resolveBdInvokingGCBinary()
	if err == nil {
		env["GC_BIN"] = gcBin
		return
	}
	bestEffortBdGCBinaryWarning.Do(func() {
		log.Printf("gc: cannot canonicalize the invoking gc executable (%v); bd callbacks will use the ambient GC_BIN or PATH gc until this process restarts", err)
	})
	if fallback := bestEffortInvokingGCBinary(); fallback != "" {
		env["GC_BIN"] = fallback
	}
}

// bestEffortInvokingGCBinary reproduces the fallback chain this branch replaced:
// the absolute path os.Executable() reports, then a gc on PATH, then nothing at
// all — in which case the caller leaves whatever GC_BIN the environment already
// carries.
//
// Each link has to name a binary that still runs. gc-beads-bd.sh treats any
// non-empty GC_BIN as authoritative (resolve_gc_helper_bin) and `die`s when the
// exec fails, so handing it the removed path the strict resolver just rejected
// turns a recover that would have completed through the script's shell-native
// fallbacks into one that SIGTERMs the managed Dolt and exits before restarting
// it. An empty GC_BIN is the better answer than a dead one.
func bestEffortInvokingGCBinary() string {
	if executable, err := resolveInvokingExecutable(); err == nil && filepath.IsAbs(executable) && runnableGCBinary(executable) {
		return executable
	}
	if found, err := exec.LookPath("gc"); err == nil && runnableGCBinary(found) {
		return found
	}
	return ""
}

// runnableGCBinary reports whether path still names an executable file. Stat
// follows symlinks, so a link whose target an upgrade removed is refused too.
func runnableGCBinary(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func warnExternalBdOverrideDrift(stderr io.Writer, cityPath string, target execStoreTarget) {
	resolved, ok, err := canonicalScopeDoltTarget(cityPath, target.ScopeRoot)
	if err != nil || !ok || !resolved.External {
		return
	}
	var drift []string
	if host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST")); host != "" && host != strings.TrimSpace(resolved.Host) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_HOST=%s (canonical %s)", host, strings.TrimSpace(resolved.Host)))
	}
	if port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT")); port != "" && port != strings.TrimSpace(resolved.Port) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_PORT=%s (canonical %s)", port, strings.TrimSpace(resolved.Port)))
	}
	if len(drift) == 0 {
		return
	}
	_, _ = fmt.Fprintf(stderr, "gc bd: warning: ignoring ambient Dolt host/port override for external target: %s\n", strings.Join(drift, ", "))
}

// rewriteBdHeartbeatArgs validates the `heartbeat <issue-id>` subcommand and
// forwards it to bd's NATIVE heartbeat, which pushes the claim's
// lease_expires_at forward and fails loudly when the caller no longer owns
// the claim (reclaimed lease, closed issue). gc used to rewrite this into
// `update <issue-id> --set-metadata gc.last_heartbeat_at=<now>` — a write
// nothing reads — which reported success while leaving the lease untouched,
// so a worker's claim could go stale mid-task under a green heartbeat
// (dip-wdt5aq). The id is validated here so a malformed id never reaches
// bd's prefix-based rig auto-detection. Args that do not begin with
// "heartbeat" pass through unchanged.
func rewriteBdHeartbeatArgs(bdArgs []string) ([]string, error) {
	if len(bdArgs) == 0 || bdArgs[0] != "heartbeat" {
		return bdArgs, nil
	}
	rest := bdArgs[1:]
	// A bead id never contains whitespace; reject any (leading, trailing, or
	// internal) rather than forwarding a malformed id that would break bd's
	// prefix-based rig auto-detection. Also reject empty and flag-shaped args.
	if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") ||
		strings.IndexFunc(rest[0], unicode.IsSpace) >= 0 {
		return nil, fmt.Errorf("usage: gc bd heartbeat <issue-id>")
	}
	return []string{"heartbeat", rest[0]}, nil
}

// bdRigQualifiedMetadataRefusal refuses an outgoing lease owner or route target
// whose rig segment is absent from the loaded city configuration. These values
// are opaque to bd, so gc bd is the common admission boundary for stale and
// external writers.
//
// Actor names without a slash remain compatible: historic dotted identities
// are provenance, not rig-qualified routes. Both bd metadata spellings are
// examined so --metadata cannot bypass the --set-metadata guard. Inputs this
// preflight cannot interpret exactly are refused before bd can mutate state.
func bdRigQualifiedMetadataRefusal(cfg *config.City, bdArgs []string) (string, bool) {
	verb, args := bdflags.SplitGlobalFlags(bdArgs)
	// bd registers `new` as an alias for `create` (bd create --help: "Aliases:
	// create, new"), so the alias has to reach the same admission check AND the
	// same flag manifest. Normalizing here covers both, because those are the
	// only two things verb is read for. The gate alone would not: bdflags keys
	// its manifests under the canonical verb only and performs no alias
	// normalization, so ValueFlags("new") is nil, and an empty manifest steps
	// over no value — the failure mode globalValueFlags' doc comment calls
	// load-bearing.
	if verb == "new" {
		verb = "create"
	}
	if verb != "create" && verb != "update" {
		return "", false
	}
	valueFlags := bdflags.ValueFlags(verb)
	configuredRigs := make(map[string]struct{}, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		configuredRigs[rig.Name] = struct{}{}
	}

	validate := func(key, value string) (string, bool) {
		if strings.HasPrefix(strings.TrimSpace(key), beadmeta.SessionRequestReceiptPrefix) {
			return fmt.Sprintf("gc bd: refusing controller-owned session request receipt metadata %q; use the tracked session protocol\n", key), true
		}
		if protectedSessionAuthorityMetadata(key) {
			return fmt.Sprintf("gc bd: refusing controller-owned session authority metadata %q; use the signed session permission-mode API\n", key), true
		}
		if beadmeta.IsGenericMutationReservedKey(key) {
			return fmt.Sprintf("gc bd: refusing protected session lifecycle metadata key %q before write\n", key), true
		}
		if key != beadmeta.LeaseOwnerMetadataKey && key != beadmeta.RoutedToMetadataKey {
			return "", false
		}
		rig, _, qualified := strings.Cut(value, "/")
		if !qualified || rig == "" {
			return "", false
		}
		if _, ok := configuredRigs[rig]; ok {
			return "", false
		}
		return fmt.Sprintf("gc bd: refusing %s=%q: rig %q is not configured in this city\n", key, value, rig), true
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		value := ""
		switch {
		case arg == "--unset-metadata":
			if i+1 >= len(args) {
				return fmt.Sprintf("gc bd: refusing %s without a value before write\n", arg), true
			}
			i++
			if msg, refused := validate(args[i], ""); refused {
				return msg, true
			}
			continue
		case strings.HasPrefix(arg, "--unset-metadata="):
			if msg, refused := validate(strings.TrimPrefix(arg, "--unset-metadata="), ""); refused {
				return msg, true
			}
			continue
		case arg == "--set-metadata" || arg == "--metadata":
			if i+1 >= len(args) {
				return fmt.Sprintf("gc bd: refusing %s without a value before write\n", arg), true
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--set-metadata="):
			value = strings.TrimPrefix(arg, "--set-metadata=")
		case strings.HasPrefix(arg, "--metadata="):
			value = strings.TrimPrefix(arg, "--metadata=")
		default:
			if !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(args) {
				i++
			}
			continue
		}

		if strings.HasPrefix(arg, "--unset-metadata") {
			key := strings.TrimSpace(value)
			if key == "" {
				return fmt.Sprintf("gc bd: refusing malformed --unset-metadata value %q before write\n", value), true
			}
			if msg, refused := validate(key, ""); refused {
				return msg, true
			}
			continue
		}

		if strings.HasPrefix(arg, "--set-metadata") {
			key, metadataValue, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return fmt.Sprintf("gc bd: refusing malformed --set-metadata value %q before write\n", value), true
			}
			if msg, refused := validate(key, metadataValue); refused {
				return msg, true
			}
			continue
		}

		metadataJSON := strings.TrimSpace(value)
		if strings.HasPrefix(metadataJSON, "@") {
			return fmt.Sprintf("gc bd: refusing --metadata %q: @file input cannot be validated before write\n", value), true
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
			return fmt.Sprintf("gc bd: refusing malformed --metadata value before write: %v\n", err), true
		}
		for key, rawValue := range metadata {
			var metadataValue string
			if err := json.Unmarshal(rawValue, &metadataValue); err != nil {
				if strings.HasPrefix(strings.TrimSpace(key), beadmeta.SessionRequestReceiptPrefix) || protectedSessionAuthorityMetadata(key) || key == beadmeta.LeaseOwnerMetadataKey || key == beadmeta.RoutedToMetadataKey {
					return fmt.Sprintf("gc bd: refusing non-string %s before write\n", key), true
				}
				continue
			}
			if msg, refused := validate(key, metadataValue); refused {
				return msg, true
			}
		}
	}
	return "", false
}

func bdMetadataMutationKeys(bdArgs []string) (map[string]struct{}, bool) {
	verb, args := bdflags.SplitGlobalFlags(bdArgs)
	if verb == "new" {
		verb = "create"
	}
	if verb != "create" && verb != "update" {
		return nil, true
	}
	valueFlags := bdflags.ValueFlags(verb)
	keys := make(map[string]struct{})
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		value := ""
		switch {
		case arg == "--unset-metadata":
			if i+1 >= len(args) {
				return nil, false
			}
			i++
			keys[strings.TrimSpace(args[i])] = struct{}{}
			continue
		case strings.HasPrefix(arg, "--unset-metadata="):
			keys[strings.TrimSpace(strings.TrimPrefix(arg, "--unset-metadata="))] = struct{}{}
			continue
		case arg == "--set-metadata" || arg == "--metadata":
			if i+1 >= len(args) {
				return nil, false
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--set-metadata="):
			value = strings.TrimPrefix(arg, "--set-metadata=")
		case strings.HasPrefix(arg, "--metadata="):
			value = strings.TrimPrefix(arg, "--metadata=")
		default:
			if !strings.Contains(arg, "=") && valueFlags[arg] && i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--set-metadata") {
			key, _, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, false
			}
			keys[strings.TrimSpace(key)] = struct{}{}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(value), "@") {
			return nil, false
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &metadata); err != nil {
			return nil, false
		}
		for key := range metadata {
			keys[strings.TrimSpace(key)] = struct{}{}
		}
	}
	return keys, true
}

func bdPersistedProtectedMutationRefusal(bdArgs, writeIDs []string, current map[string]beads.Bead) (string, bool) {
	verb, verbArgs, _ := bdRelocatedClassVerb(bdArgs)
	if verb == "delete" && bdCascadeDeleteRequested(verbArgs) {
		return "gc bd: refusing cascade delete because the full dependency closure cannot be checked for retained session evidence\n", true
	}
	for id, bead := range current {
		if verb == "delete" && beads.HasRetainedSessionRequestEvidence(bead) {
			return fmt.Sprintf("gc bd: refusing delete of %q because its session request evidence has no deletion policy\n", id), true
		}
	}
	keys, ok := bdMetadataMutationKeys(bdArgs)
	if !ok {
		return "gc bd: cannot safely inspect protected metadata mutation before write\n", true
	}
	requiresCurrent := verb == "delete"
	for key := range keys {
		if sessionAuthorityOptionMetadata(key) {
			requiresCurrent = true
		}
	}
	if requiresCurrent {
		for _, id := range writeIDs {
			if _, ok := current[id]; !ok {
				return fmt.Sprintf("gc bd: refusing protected mutation of %q because its current metadata could not be verified\n", id), true
			}
		}
	}
	for id, bead := range current {
		for key := range keys {
			if sessionauthority.ProtectsMetadataMutation(key, bead.Metadata) {
				return fmt.Sprintf("gc bd: refusing controller-owned session authority metadata %q on protected bead %q; use the signed session permission-mode API\n", key, id), true
			}
		}
	}
	return "", false
}

func bdCascadeDeleteRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--cascade" {
			return true
		}
		value, ok := strings.CutPrefix(arg, "--cascade=")
		if !ok {
			continue
		}
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil || enabled {
			return true
		}
	}
	return false
}

func protectedSessionAuthorityMetadata(key string) bool {
	return sessionauthority.ProtectsMetadataMutation(key, nil)
}

func sessionAuthorityOptionMetadata(key string) bool {
	switch strings.TrimSpace(key) {
	case sessionauthority.MetadataTemplateOverrides, sessionauthority.MetadataPermissionModeOption:
		return true
	default:
		return false
	}
}

func sessionAuthorityMetadataPreviouslyProtected(metadata map[string]string) bool {
	return sessionauthority.HasAuthorityMetadata(metadata)
}

func doBd(args []string, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	bdArgs, err := rewriteBdHeartbeatArgs(bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// A `list` that filters on the wisps tier reaches that tier only with
	// --include-infra; without it bd answers [] and exit 0 on a ledger full of
	// molecules. See bd_wisp_tier.go.
	bdArgs = rewriteBdWispTierArgs(bdArgs)

	// Refuse a dropped --set-metadata pair before any store work, so nothing is
	// written and the exit code is honest. bd applies the subset and exits 0.
	if msg, mistyped := mistypedMetadataPairRefusal(bdArgs); mistyped {
		fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use the full config load path (includes pack expansion + site
	// binding overlay) so migrated rigs (path only in .gc/site.toml)
	// resolve to their bound path. A raw config.Load here would make
	// every already-migrated rig look unbound and fail the new guard
	// in resolveBdScopeTarget / bdRigScopeTarget.
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cfg.Lifecycle.AdmissionEnabled && !lifecycleAdmissionBdCommandReadOnly(bdArgs) {
		fmt.Fprintf(stderr, "gc bd: lifecycle admission is enabled; refusing raw bd command %q because it has no lifecycle proof-aware write guard\n", bdArgs) //nolint:errcheck // best-effort stderr
		return 1
	}
	if msg, refused := bdRigQualifiedMetadataRefusal(cfg, bdArgs); refused {
		fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}

	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs, cityName != "", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// `gc bd sql`, `gc bd query` and the selector verbs (`list`, `search`) are
	// passthroughs to bd, and bd answers about the bd ledger only. On a split
	// city a read that names a relocated class's beads comes back empty and exit
	// 0 — a confident wrong answer, and the one that reported live molecule roots
	// as missing. A frontier read (`gc bd ready`, or `gc bd list --ready`, which
	// runs the same query) is refused on the same seam for a different reason:
	// its whole result set is short by the relocated class whatever the argv.
	// Refuse both here, where the class routing is known; bd cannot know a class
	// was relocated.
	if msg, blind := bdSQLRelocatedClassRefusal(cfg, bdArgs); blind {
		if !bdRelocatedClassOverrideEnabled() {
			fmt.Fprintf(stderr, "gc bd: %s.%s\n", msg, bdRelocatedClassEscapeHint(bdRelocatedClassInvocationComputesFrontier(bdArgs))) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Overridden, but never silently: the operator asked for a read this
		// ledger cannot answer by class, so the reason it would have been
		// refused travels with the result they are about to trust.
		fmt.Fprintf(stderr, "gc bd: %s is set; running anyway: %s\n", bdRelocatedClassOverrideEnvVar, msg) //nolint:errcheck // best-effort stderr
	}
	// The same split, on the write side. `gc bd create` is a passthrough too, and
	// bd writes the work ledger only, so a create whose SHAPE belongs to a
	// relocated class strands the bead in a ledger that class is never read from
	// — silently, because bd did what it was asked and exited 0. Placement is
	// impossible here (only argv crosses to the subprocess), so the create is
	// refused before anything is written and the refusal names the gc-native
	// command that mints it correctly.
	//
	// The read override above deliberately does not reach this arm: it exists
	// because the read scan classifies ambiguous TEXT and a refused read can be
	// re-run, while a stranded mint leaves a row under the wrong prefix that no
	// later read finds and no migration moves.
	//
	// It runs before the by-id door rather than after because the two answer
	// different questions and cannot shadow each other: this arm reads the
	// prospective bead's CLASS and never an addressed id, so a create that names
	// a relocated bead in --parent still reaches the ownership refusal, which
	// names that bead. When both are true the mint is what has to be stopped.
	if msg, stranded := bdRelocatedClassCreateRefusal(cfg, target.ScopeRoot, bdArgs); stranded {
		fmt.Fprintf(stderr, "gc bd: %s\n", msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	// A by-ID operation whose subject a relocated class owns is answered in
	// process, from the binding that class is served from, and never handed to
	// the subprocess — which opens the work workspace and cannot see the bead.
	// It runs BEFORE the release-if-current arm below because that arm resolves
	// only the work scope: on a split city it would release against the ledger
	// the bead was moved off. See cmd_bd_by_id.go.
	//
	// rigName is the explicit --rig, and it travels because the WORK scope this
	// function just resolved and the class binding are two different ledgers: a
	// class-owned subject under an explicit --rig is refused rather than served
	// from a store the operator did not name. Auto-detected scope (GC_RIG, -C,
	// cwd) is resolved inside resolveBdScopeTarget and deliberately does not
	// travel — see refuseRigScopedClassOwnedTarget.
	if code, handled := maybeRouteBdByID(cityPath, rigName, bdArgs, stdout, stderr); handled {
		return code
	}
	if id, expectedAssignee, ok, err := parseBdReleaseIfCurrentArgs(bdArgs); ok || err != nil {
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return doBdReleaseIfCurrent(cityPath, cfg, target, id, expectedAssignee, stdout, stderr)
	}

	// Disclose which store answers a read-only passthrough, so a zero-row
	// result is distinguishable from a true empty (gastownhall/gascity#5170).
	// resolveBdScopeTarget's priority chain (explicit --rig > explicit --city
	// > bead-prefix detect > -C/--directory > GC_RIG env > cwd > city)
	// silently picks a store on every one of those paths but the GC_RIG-
	// mismatch warning above; the common cwd-auto-detect case reached bd with
	// no diagnostic at all. Placed after the by-ID and release-if-current
	// arms above (rather than immediately after resolveBdScopeTarget) so it
	// names the store that actually serves the request: a class-owned `show`
	// on a split city is answered in process from the class's own binding by
	// maybeRouteBdByID, not from target, and disclosing target there would be
	// wrong for that one read. The disclosure is stderr-only and additive; it
	// never changes the exit code or the separate private-record projection.
	if verb, _, ok := bdRelocatedClassVerb(bdArgs); ok && bdScopeDisclosureVerbs[verb] {
		fmt.Fprintf(stderr, "gc bd: answering from the %s store\n", scopeLabel(target)) //nolint:errcheck // best-effort stderr
	}

	if provider := rawBeadsProviderForScope(target.ScopeRoot, cityPath); !providerUsesBdStoreContract(provider) {
		fmt.Fprintf(stderr, "gc bd: only supported for bd-backed beads providers (resolved %q for %s)\n", provider, target.ScopeRoot) //nolint:errcheck // best-effort stderr
		if hint := bdProviderMismatchHint(target.ScopeRoot, provider); hint != "" {
			fmt.Fprintf(stderr, "  hint: %s\n", hint) //nolint:errcheck // best-effort stderr
		}
		return 1
	}

	// Exact generic reads and deletes stay inside the typed store boundary.
	// Reads are redacted by printBdByIDBead; deletes use request-evidence and
	// revision guards instead of falling through to raw bd.
	if op, ok := parseBdByIDOp(bdArgs); ok && op.Verb == bdByIDDelete {
		store, openErr := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
		if openErr != nil {
			fmt.Fprintf(stderr, "gc bd: opening store for %s: %v\n", op.Verb, openErr) //nolint:errcheck
			return 1
		}
		b, getErr := store.Get(op.ID)
		if getErr != nil {
			fmt.Fprintf(stderr, "gc bd: %s %s: %v\n", op.Verb, op.ID, getErr) //nolint:errcheck
			return 1
		}
		if session.IsSessionBeadOrRepairable(b) || session.HasRequestEvidence(b) {
			return doBdByIDDelete(store, b, op, stdout, stderr)
		}
		// Ordinary work deletion retains bd's established output and semantics.
	}

	// Pre-flight exact-ID guard for write-mutating subcommands (gcy-g4o).
	// bd's fuzzy/substring resolver can silently match a longer ID that
	// contains the supplied ID as a substring (e.g. "gcy-dv7" → "gcy-wisp-dv78").
	// Verify via BdStore.Get — which already enforces an exact-ID match —
	// before forwarding any mutation to the bd subprocess.
	//
	// Fail-closed: if the arg scanner reports ambiguity (unrecognized
	// value-consuming flag), the command is rejected rather than forwarded
	// unguarded.
	//
	// Lifecycle-enrolled writes also fail closed here because the raw bd
	// subprocess has no authenticated session/claim proof or observed revision.
	// A genuinely absent bead may still be forwarded to bd so its own not-found
	// behavior remains intact; an unavailable read seam cannot establish that
	// the target is unenrolled, so it is refused.
	//
	// Note: gc bd show (read passthrough) does NOT have this guard and still
	// substring-resolves. That is intentional — reads are non-destructive.
	//
	// guardStore/guardBeads capture the store this guard opens and the beads
	// it reads so the work-record close gate below can reuse them instead of
	// opening the store and re-fetching the same bead a second time.
	var (
		guardStore beads.Store
		guardBeads map[string]beads.Bead
		guardIDs   []string
	)
	if writeIDs, writeOK, ambiguous := bdMutationWriteIDs(bdArgs); writeOK {
		guardIDs = writeIDs
		if ambiguous {
			fmt.Fprintf(stderr, "gc bd: cannot safely verify bead IDs (unrecognized flag in args %v); aborting to prevent substring-resolution mutation of the wrong bead\n", bdArgs) //nolint:errcheck // best-effort stderr
			return 1
		}
		if len(writeIDs) > 0 {
			store, storeErr := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
			if storeErr != nil {
				fmt.Fprintf(stderr, "gc bd: cannot verify lifecycle enrollment for write target: %v; refusing mutation\n", storeErr) //nolint:errcheck // best-effort stderr
				return 1
			}
			guardStore = store
			guardBeads = make(map[string]beads.Bead, len(writeIDs))
			// Bulk exact reads are an optimization, never a substitute for
			// checking every target and its lifecycle admission below.
			var batch map[string]beads.Bead
			if getter, ok := store.(beads.ExactBatchGetter); ok && len(writeIDs) > 1 {
				found, _, batchErr := getter.GetExactBatch(writeIDs)
				if batchErr == nil {
					batch = found
				} else if !errors.Is(batchErr, beads.ErrExactBatchGetUnsupported) {
					fmt.Fprintf(stderr, "gc bd: cannot verify lifecycle enrollment for batch: %v; refusing mutation\n", batchErr) //nolint:errcheck
					return 1
				}
			}
			for _, id := range writeIDs {
				bead, found := batch[id]
				var getErr error
				if !found {
					bead, getErr = store.Get(id)
				} else if bead.ID != id {
					getErr = beads.ErrIDCollision
				}
				if errors.Is(getErr, beads.ErrIDCollision) {
					// bd resolved a different bead — block the write to prevent
					// mutating the wrong bead via substring resolution.
					fmt.Fprintf(stderr, "gc bd: bead %q resolved to a different bead ID (substring collision); aborting to prevent mutating the wrong bead\n", id) //nolint:errcheck // best-effort stderr
					return 1
				}
				if errors.Is(getErr, beads.ErrNotFound) {
					continue
				}
				if getErr != nil {
					fmt.Fprintf(stderr, "gc bd: cannot verify lifecycle enrollment for %q: %v; refusing mutation\n", id, getErr) //nolint:errcheck // best-effort stderr
					return 1
				}
				if err := worklifecycle.ValidateGenericMutation(bead); err != nil {
					fmt.Fprintf(stderr, "gc bd: %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
					return 1
				}
				guardBeads[id] = bead
			}
			if bdArgs[0] == "delete" {
				for _, bead := range guardBeads {
					if session.IsSessionBeadOrRepairable(bead) || session.HasRequestEvidence(bead) {
						fmt.Fprintf(stderr, "gc bd: delete %q requires one exact ID so session request evidence can be protected\n", bead.ID) //nolint:errcheck
						return 1
					}
				}
			}
		}
	}
	if msg, refused := bdPersistedProtectedMutationRefusal(bdArgs, guardIDs, guardBeads); refused {
		fmt.Fprint(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Work-record close gate (ADR-0009): a close routed through the SDK seam
	// must satisfy the typed work-record contract (gc.work_outcome present;
	// shipped ⇒ gc.work_commit reachable on gc.work_branch). Warn-only by default;
	// blocks the close only when GC_WORK_RECORD_ENFORCE is set. Reuses the
	// store/beads the write-ID guard above already opened and read, and the
	// config the caller already loaded.
	if gateExitCode := runWorkRecordCloseGate(bdArgs, target.ScopeRoot, cityPath, cfg, guardStore, guardBeads, stderr); gateExitCode != 0 {
		return gateExitCode
	}
	if ids, reason, jsonOutput, ok := parseFencedBatchClose(bdArgs); ok {
		written, err := closeBdBatchAtRevisions(guardStore, guardBeads, ids, reason)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: batch close: %v\n", err) //nolint:errcheck
			return 1
		}
		if jsonOutput {
			if err := writeJSON(stdout, bridgeBeads(written)); err != nil {
				fmt.Fprintln(stderr, err) //nolint:errcheck
				return 1
			}
		} else {
			for _, b := range written {
				fmt.Fprintf(stdout, "Closed %s\n", b.ID) //nolint:errcheck
			}
		}
		return 0
	}
	if op, ok := parseBdByIDOp(bdArgs); ok && (op.Verb == bdByIDUpdate || op.Verb == bdByIDClose || op.Verb == bdByIDReopen) {
		store := guardStore
		if store == nil {
			goto protectedChecks
		}
		current, found := guardBeads[op.ID]
		if !found {
			goto protectedChecks
		}
		materializeBdByIDReplacementLabels(current, &op)
		var writeErr error
		switch op.Verb {
		case bdByIDUpdate:
			writeErr = updateBdStoreBridgeAtRevision(store, current, op.Update)
		case bdByIDClose:
			closed := "closed"
			writeErr = updateBdStoreBridgeAtRevision(store, current, beads.UpdateOpts{Status: &closed})
		case bdByIDReopen:
			open := "open"
			opts := beads.UpdateOpts{Status: &open}
			if current.Status != "closed" {
				writeErr = fmt.Errorf("bead is not closed (status: %s)", current.Status)
			} else {
				writeErr = updateBdStoreBridgeAtRevision(store, current, opts)
			}
		}
		if writeErr != nil {
			fmt.Fprintf(stderr, "gc bd: %s %s: %v\n", op.Verb, op.ID, writeErr) //nolint:errcheck
			return 1
		}
		written, err := store.Get(op.ID)
		if err != nil {
			fmt.Fprintf(stderr, "gc bd: %s %s was written and could not be re-read: %v\n", op.Verb, op.ID, err) //nolint:errcheck
			return 1
		}
		return printBdByIDBead(written, op.JSON, scopeLabel(target), stdout, stderr)
	}

protectedChecks:
	if len(bdArgs) > 0 {
		if bdRequestsProtectedMutation(bdArgs) && len(guardBeads) > 0 {
			fmt.Fprintln(stderr, "gc bd: refusing unrevisioned protected mutation; use a supported exact-ID status, type, or metadata form") //nolint:errcheck
			return 1
		}
		for _, bead := range guardBeads {
			if session.IsRequestPurgeFenced(bead) {
				fmt.Fprintf(stderr, "gc bd: refusing unrevisioned %s of purge-fenced bead %q\n", bdArgs[0], bead.ID) //nolint:errcheck
				return 1
			}
			if bdArgs[0] == "update" && session.HasRequestEvidence(bead) && bdUpdateRequestsTypeChange(bdArgs[1:]) {
				fmt.Fprintf(stderr, "gc bd: refusing issue-type change of receipt-bearing bead %q\n", bead.ID) //nolint:errcheck
				return 1
			}
		}
	}

	reapStaleBdExportJSONL(target.ScopeRoot)
	warnExternalBdOverrideDrift(stderr, cityPath, target)

	// Resolve the same binary every other bd path in the tree resolves for
	// this scope: the city scope, any rig that inherits the city backend, and
	// any scope bound to a complete storage binding all pin the bd build that
	// speaks that backend, and the passthrough must honor the pin or it hands
	// the command to an ambient bd that rejects the bound backend. Keying on
	// the target scope rather than the city keeps a rig that overrides the
	// city backend, and owns no binding of its own, on the ambient bd its
	// runtime env already implies.
	bdPath, err := resolveBdBinaryForScope(cityPath, target.ScopeRoot)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	projectOutput, err := bdPresentationCommand(bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck
		return 1
	}
	jsonOutput := bdPresentationJSONRequested(bdArgs)
	providerArgs := append([]string(nil), bdArgs...)
	if projectOutput && !jsonOutput {
		providerArgs = bdJSONProviderArgs(bdArgs)
	}
	cmd := exec.Command(bdPath, providerArgs...)
	cmd.Dir = target.ScopeRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	var presentationOutput bytes.Buffer
	var credentialOutput bytes.Buffer
	if projectOutput {
		cmd.Stdout = &presentationOutput
	} else if bdOutputNeedsCredentialRedaction(bdArgs) {
		cmd.Stdout = &credentialOutput
	}
	// Tee stderr through a bounded head buffer alongside the operator's
	// pipe so we can scan it post-exec for bd's silent-fallback-to-on-disk
	// marker. Only stderr is teed: bd writes its auto-import banner there,
	// not to stdout. See gastownhall/gascity#2080 (update path) and #2079
	// (close path) — both go through this handoff.
	stderrScan := &headLimitedWriter{limit: bdStderrScanLimit}
	cmd.Stderr = io.MultiWriter(stderr, stderrScan)
	if projectOutput {
		cmd.Stderr = stderrScan
	}
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cmd.Env = workQueryEnvForDir(env, cmd.Dir)

	// GC serves every recognized watch using bounded JSON reads. Native bd
	// watch rendering cannot apply the private-record projection, while proxied
	// bd refuses watch mode altogether. Both scopes retain change polling,
	// cancellation, tracing and managed-store diagnostics without raw egress.
	if req, ok := parseBdShowWatchArgs(bdArgs); ok {
		return serveBdShowWatch(req, &bdWatchRunner{
			bdPath: bdPath, dir: cmd.Dir, env: cmd.Env,
			cityPath: cityPath, scopeRoot: target.ScopeRoot, stderr: stderr,
		}, stdout, stderr)
	}

	traceStart := time.Now()
	runErr := cmd.Run()
	if credentialOutput.Len() > 0 {
		if redacted, ok := redactBdJSONOutput(credentialOutput.Bytes()); ok {
			_, _ = stdout.Write(redacted)
		} else {
			_, _ = stdout.Write(redactBdTextOutput(credentialOutput.Bytes()))
		}
	}
	traceExit := 0
	if runErr != nil {
		if projectOutput {
			fmt.Fprintln(stderr, "gc bd: provider command failed; private diagnostic withheld") //nolint:errcheck
			if bdOutputSuggestsConflictingDoltStart(stderrScan.String()) {
				fmt.Fprintln(stderr, "gc bd: provider suggested bd dolt start") //nolint:errcheck
			}
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			traceExit = exitErr.ExitCode()
		} else {
			traceExit = -1
		}
	}
	beads.TraceBDCall("go:gc-bd-passthrough", target.ScopeRoot, bdArgs, traceStart, traceExit, runErr)

	if runErr != nil {
		if traceExit > 0 {
			if bdOutputSuggestsConflictingDoltStart(stderrScan.String()) &&
				bdScopeDoltIsGcManaged(cityPath, target.ScopeRoot) {
				fmt.Fprintln(stderr, bdDoltStartConflictUserMessage) //nolint:errcheck // best-effort stderr
			}
			return traceExit
		}
		fmt.Fprintf(stderr, "gc bd: %v\n", runErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	// bd exited 0 — but if its stderr shows the silent fallback to on-disk
	// auto-import, the managed Dolt server was unreachable and any write in
	// this command was dropped (managed Gas City sets BD_EXPORT_AUTO=false;
	// see applyExportSuppressionEnv in cmd/gc/bd_env.go). Surface that as a
	// hard error instead of a misleading exit 0. One check here covers the
	// whole bd-write-persistence quad (gastownhall/gascity#2079 / #2080 /
	// #2149 / #2150) because every bd subcommand routes through this
	// handoff. A non-zero bd exit is intentionally left to the block above:
	// the existing transport-retry classifier already handles the
	// timeout+marker case, and overriding a real bd exit code here would
	// mask it. (Root cause fixed upstream in beads post-#3691; this surfaces
	// the symptom for deployments still on stable bd builds.)
	if bdOutputIndicatesSilentFallback(stderrScan.String()) {
		if projectOutput {
			fmt.Fprintln(stderr, "gc bd: provider auto-importing fallback detected; private diagnostic withheld") //nolint:errcheck
		}
		fmt.Fprintln(stderr, bdSilentFallbackUserMessage) //nolint:errcheck // best-effort stderr
		return bdSilentFallbackExitCode
	}

	if projectOutput {
		return writeBdProviderPresentation(presentationOutput.Bytes(), jsonOutput, stdout, stderr)
	}
	return 0
}

// lifecycleAdmissionBdCommandReadOnly allows a narrow set of raw bd reads
// while lifecycle admission is enabled. The raw passthrough has no proof-aware
// write guard, so every unknown command and every mutation alias must fail
// closed. Parse persistent flags strictly so an unknown leading flag cannot
// make its value look like an allowlisted read verb.
func lifecycleAdmissionBdCommandReadOnly(args []string) bool {
	globalValueFlags := bdflags.GlobalValueFlags()
	globalBoolFlags := bdflags.GlobalBoolFlags()
	verb := ""
	rootHelpOrVersion := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if !strings.HasPrefix(arg, "-") {
			verb = arg
			break
		}
		name := arg
		inline := false
		if at := strings.IndexByte(arg, '='); at >= 0 {
			name, inline = arg[:at], true
		}
		if globalValueFlags[name] {
			if !inline {
				if i+1 >= len(args) {
					return false
				}
				i++
			}
			continue
		}
		if globalBoolFlags[name] {
			if name == "--help" || name == "-h" || name == "--version" || name == "-V" {
				rootHelpOrVersion = true
			}
			continue
		}
		return false
	}
	if verb == "" {
		return rootHelpOrVersion
	}
	switch verb {
	case "show", "list", "search", "query", "count", "ready", "blocked",
		"children", "status", "statuses", "types", "history", "diff", "stale",
		"graph", "stats", "info", "where", "context", "version", "help":
		return true
	default:
		// This includes nested command families, aliases, raw SQL, the internal
		// release-if-current verb, and commands added by a future bd version.
		// A future read verb can be added here with a direct bounded read contract.
		return false
	}
}

// bdRequestsProtectedMutation reports whether an update changes a field whose
// authorization depends on the inspected row, or whether the verb itself is a
// lifecycle transition. Callers that cannot parse such a mutation into an
// UpdateIfMatch must refuse it rather than hand an unrevisioned write to bd.
func bdRequestsProtectedMutation(args []string) bool {
	verb, rest := bdflags.SplitGlobalFlags(args)
	if verb == "close" || verb == "reopen" {
		return true
	}
	if verb != "update" {
		return false
	}
	valueFlags := bdflags.ValueFlags("update")
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, inline := strings.Cut(arg, "=")
		switch name {
		case "--title", "--status", "-s", "--type", "-t", "--priority", "-p",
			"--description", "-d", "--assignee", "-a", "--add-label", "--remove-label", "--set-labels",
			"--metadata", "--set-metadata", "--unset-metadata":
			return true
		}
		if !inline && valueFlags[name] && i+1 < len(rest) {
			i++
		}
	}
	return false
}

func bdUpdateRequestsTypeChange(args []string) bool {
	for _, arg := range args {
		if arg == "--type" || arg == "-t" || strings.HasPrefix(arg, "--type=") || strings.HasPrefix(arg, "-t=") {
			return true
		}
	}
	return false
}

func bdRequestsJSONOutput(args []string) bool {
	return bdflags.RequestsJSONOutput(args)
}

func bdOutputNeedsCredentialRedaction(args []string) bool {
	verb, _ := bdflags.SplitGlobalFlags(args)
	return bdRequestsJSONOutput(args) || verb == "show"
}

func redactBdTextOutput(data []byte) []byte {
	lines := bytes.SplitAfter(data, []byte("\n"))
	out := make([]byte, 0, len(data))
	credentialKeys := []string{
		beadmeta.SessionInstanceTokenMetadataKey,
		"execution_token",
		beadmeta.Namespace + "instance_token",
		beadmeta.Namespace + "execution_token",
	}
	for _, line := range lines {
		credential := false
		for _, key := range credentialKeys {
			if bdTextContainsMetadataKey(string(line), key) {
				credential = true
				break
			}
		}
		if !credential {
			out = append(out, line...)
		}
	}
	return out
}

func bdTextContainsMetadataKey(line, key string) bool {
	for start := 0; ; {
		index := strings.Index(line[start:], key)
		if index < 0 {
			return false
		}
		end := start + index + len(key)
		if end == len(line) || !unicode.IsLetter(rune(line[end])) && !unicode.IsDigit(rune(line[end])) && line[end] != '_' && line[end] != '.' {
			return true
		}
		start = end
	}
}

func redactBdJSONOutput(data []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	decoded := false
	for {
		var value any
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				return output.Bytes(), decoded
			}
			return nil, false
		}
		redactBdJSONValue(value)
		if err := encoder.Encode(value); err != nil {
			return nil, false
		}
		decoded = true
	}
}

func redactBdJSONValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if beadmeta.IsExecutionCredentialMetadataKey(key) {
				delete(typed, key)
				continue
			}
			redactBdJSONValue(child)
		}
	case []any:
		for _, child := range typed {
			redactBdJSONValue(child)
		}
	}
}

func parseBdReleaseIfCurrentArgs(args []string) (id, expectedAssignee string, ok bool, err error) {
	if len(args) == 0 || args[0] != "release-if-current" {
		return "", "", false, nil
	}
	if len(args) != 3 || invalidBdReleaseIfCurrentArg(args[1]) || invalidBdReleaseIfCurrentArg(args[2]) {
		return "", "", true, fmt.Errorf("usage: gc bd release-if-current <issue-id> <assignee>")
	}
	return args[1], args[2], true, nil
}

func invalidBdReleaseIfCurrentArg(value string) bool {
	return value == "" || strings.IndexFunc(value, unicode.IsSpace) >= 0
}

// bdMutationWriteIDs extracts all positional bead IDs from a bd write-mutation
// command (update, close, reopen, delete, heartbeat) and reports whether the
// scan was unambiguous.
//
// Returns:
//   - ids: all positional (non-flag) tokens after the subcommand; may be empty.
//   - ok: false if args is empty or the subcommand is not a write-mutation.
//   - ambiguous: true if the scanner encountered an unrecognized flag that
//     might consume the next argument as its value. In that case the caller
//     must fail-closed — forwarding the command unguarded risks the original
//     substring-resolution bug (gcy-g4o).
//
// The scanner has complete knowledge of every value-consuming flag for each
// subcommand (sourced from `bd <sub> --help`). Unknown flags that start with
// "-" and do not contain "=" are treated as potentially value-consuming, which
// triggers ambiguous=true. Boolean flags (no value) are fine to ignore.
// The "--" terminator is respected: everything after it is positional.
// heartbeat is positional-only — rewriteBdHeartbeatArgs has already reduced its
// argv to a single pre-validated id with no flags, so its flag sets are empty
// by design and the lone id is scanned as positional.
//
// All returned IDs must be verified via BdStore.Get (exact-ID guard) before
// the mutation is forwarded to the bd subprocess.
func bdMutationWriteIDs(args []string) (ids []string, ok bool, ambiguous bool) {
	if len(args) == 0 {
		return nil, false, false
	}
	sub := args[0]
	switch sub {
	case "update", "close", "reopen", "delete", "heartbeat":
	default:
		return nil, false, false
	}

	// valueFlags is the complete set of flags that consume the next argument as
	// their value for this subcommand, in both long and short form.
	// Sourced from `bd <sub> --help` (bd 1.3.1-rc.2, 2026-09-29).
	valueFlags := bdSubcmdValueFlags(sub)

	// boolFlags is the complete set of boolean (no-value) flags. Unknown flags
	// not in either set trigger ambiguous=true.
	boolFlags := bdSubcmdBoolFlags(sub)

	positional := false // true after "--"
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if positional {
			if arg != "" {
				ids = append(ids, arg)
			}
			continue
		}
		if arg == "--" {
			positional = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			// Positional token — a bead ID (or batch of IDs).
			if arg != "" {
				ids = append(ids, arg)
			}
			continue
		}
		// Flag token.
		// --flag=value form: value is embedded, no next-arg consumed.
		if strings.Contains(arg, "=") {
			continue
		}
		// Strip leading dashes to get the flag name for lookup.
		flagName := strings.TrimLeft(arg, "-")
		// Reconstruct the canonical long or short form for set membership.
		longForm := "--" + flagName
		shortForm := "-" + flagName // only meaningful when flagName is 1 char

		if valueFlags[longForm] || (len(flagName) == 1 && valueFlags[shortForm]) {
			// Known value-consuming flag: skip its value argument.
			i++
			continue
		}
		if boolFlags[longForm] || (len(flagName) == 1 && boolFlags[shortForm]) {
			// Known boolean flag: no value to skip.
			continue
		}
		// Unknown flag. It might consume a value argument that looks like a
		// bead ID. Fail-closed: report ambiguity so the caller can reject.
		return nil, true, true
	}
	return ids, true, false
}

// bdSubcmdValueFlags returns the set of value-consuming flag names (in
// "--long" / "-s" form) for the given bd write-mutation subcommand. Backed
// by internal/bdflags, the single source of truth shared with the `gc
// lint` bd-flag validation check, so the two cannot drift apart.
func bdSubcmdValueFlags(sub string) map[string]bool {
	return bdflags.ValueFlags(sub)
}

// bdSubcmdBoolFlags returns the set of boolean (no-value) flag names for the
// given bd write-mutation subcommand. Backed by internal/bdflags, the
// single source of truth shared with the `gc lint` bd-flag validation
// check, so the two cannot drift apart.
func bdSubcmdBoolFlags(sub string) map[string]bool {
	return bdflags.BoolFlags(sub)
}

// bdMutationWriteID is a compatibility shim retained for callers that only
// need the first ID. Prefer bdMutationWriteIDs for new code.
func bdMutationWriteID(args []string) (string, bool) {
	ids, ok, ambiguous := bdMutationWriteIDs(args)
	if !ok || ambiguous || len(ids) == 0 {
		return "", false
	}
	return ids[0], true
}

func doBdReleaseIfCurrent(cityPath string, cfg *config.City, target execStoreTarget, id, expectedAssignee string, stdout, stderr io.Writer) int {
	store, err := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd release-if-current: opening store: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	current, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := worklifecycle.ValidateGenericMutation(current); err != nil {
		fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		fmt.Fprintf(stderr, "gc bd release-if-current: %v for %T\n", beads.ErrConditionalReleaseUnsupported, store) //nolint:errcheck // best-effort stderr
		return 1
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil {
		if errors.Is(err, beads.ErrBDSilentFallback) {
			fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
			fmt.Fprintln(stderr, bdSilentFallbackUserMessage)          //nolint:errcheck // best-effort stderr
			return bdSilentFallbackExitCode
		}
		fmt.Fprintf(stderr, "gc bd release-if-current: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if released {
		fmt.Fprintln(stdout, "released") //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintln(stdout, "skipped") //nolint:errcheck // best-effort stdout
	return 0
}

func resolveBdCity(cityName string) (string, error) {
	if strings.TrimSpace(cityName) != "" {
		return validateCityPath(cityName)
	}
	return resolveCity()
}

// extractBdScopeFlags extracts gc-owned --city/--rig flags from the raw
// argument list and returns the requested city, rig, and remaining bd args.
// It also falls back to cobra's persistent globals for "gc --city X --rig Y bd".
func extractBdScopeFlags(args []string) (string, string, []string) {
	var cityName string
	var rigName string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--city" && i+1 < len(args):
			cityName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--city="):
			cityName = strings.TrimPrefix(args[i], "--city=")
			continue
		case args[i] == "--rig" && i+1 < len(args):
			rigName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--rig="):
			rigName = strings.TrimPrefix(args[i], "--rig=")
			continue
		}
		rest = append(rest, args[i])
	}
	if cityName == "" && cityFlag != "" {
		cityName = cityFlag
	}
	if rigName == "" && rigFlag != "" {
		rigName = rigFlag
	}
	return cityName, rigName, rest
}

// extractRigFlag extracts --rig <name> from the argument list and returns
// the rig name and remaining args. Also checks the global rigFlag set by
// cobra's persistent flag parsing (for "gc --rig foo bd list" syntax).
func extractRigFlag(args []string) (string, []string) {
	_, rigName, rest := extractBdScopeFlags(args)
	return rigName, rest
}

// extractBdDirectoryFlag returns the -C / --directory value from bd passthrough
// args, or "" if not present. The flag is left in args so bd itself still sees it.
func extractBdDirectoryFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-C" || args[i] == "--directory") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(args[i], "--directory="):
			return strings.TrimPrefix(args[i], "--directory=")
		}
	}
	return ""
}

// resolveBdScopeTarget determines the canonical scope root for a bd command.
// Priority: explicit rig name > explicit city > bead prefix auto-detection > -C dir rig match > GC_RIG env > enclosing rig > city root.
//
// stderr receives a best-effort warning when a set-but-unresolvable GC_RIG is
// discarded (see the GC_RIG block below); pass io.Discard when the caller does
// not care.
func resolveBdScopeTarget(cfg *config.City, cityPath, rigName string, args []string, cityExplicit bool, stderr io.Writer) (execStoreTarget, error) {
	resolveRigPaths(cityPath, cfg.Rigs)
	if rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return execStoreTarget{}, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before scoping bd commands", rig.Name, rig.Name)
		}
		return bdRigScopeTarget(cityPath, rig), nil
	}

	cityTarget := bdCityScopeTarget(cityPath, cfg)

	// An explicit --city pins the city store, symmetric with explicit --rig:
	// a deliberate city scope must never be silently downgraded to a rig store
	// by bead-prefix / GC_RIG-env / cwd auto-detection below. Without this,
	// `gc bd --city <path> list` returned cwd/rig-scoped results, mis-scoping
	// scripts that trusted the flag. (gastownhall/gascity#3410)
	if cityExplicit {
		return cityTarget, nil
	}

	cityPrefix := config.EffectiveHQPrefix(cfg)
	if cityPrefix != "" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") || beadPrefix(cfg, arg) != cityPrefix {
				continue
			}
			if bdBeadExists(cityPath, cfg, cityTarget, arg) {
				return cityTarget, nil
			}
		}
	}

	// Auto-detect from bead IDs in args, but only accept candidates that
	// actually exist in the resolved rig store. This keeps hyphenated flag
	// values and other non-ID args from silently retargeting the command.
	// Unbound rigs are skipped so we don't alias them to the city store.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if rig, ok := bdRigForArg(cfg, arg); ok {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			target := bdRigScopeTarget(cityPath, rig)
			if bdBeadExists(cityPath, cfg, target, arg) {
				return target, nil
			}
		}
	}

	// Honor -C / --directory passed to bd: if it names a path inside a
	// registered rig, use that rig's store. This lets `gc bd create -C
	// /path/to/packs-rig ...` route to the packs rig even when GC_RIG
	// or cwd point elsewhere. The flag stays in bdArgs so bd itself still
	// sees it and changes directory accordingly.
	if cdDir := extractBdDirectoryFlag(args); cdDir != "" {
		if rig, ok, err := resolveRigForDir(cfg, cityPath, cdDir); err != nil {
			return execStoreTarget{}, err
		} else if ok {
			return bdRigScopeTarget(cityPath, rig), nil
		}
	}

	// Honor GC_RIG env (set by the controller on every rig agent) when no
	// explicit --rig flag was given and no bead-ID in the args matched a
	// specific store. This is a weaker signal than an explicit flag or a
	// bead-prefix hit, but a stronger default than cwd: the controller sets
	// GC_RIG reliably, while cwd detection fails for polecat worktrees (they
	// live under .gc/worktrees/, not the configured rig path).
	// Priority: explicit --rig > bead-prefix detect > GC_RIG env > cwd > city.
	gcRigDiscarded := ""
	if gcRig := strings.TrimSpace(os.Getenv("GC_RIG")); gcRig != "" {
		if rig, ok := rigByName(cfg, gcRig); ok && strings.TrimSpace(rig.Path) != "" {
			return bdRigScopeTarget(cityPath, rig), nil
		}
		// GC_RIG names an unknown or unbound rig. Unlike an explicit --rig
		// (which exits 1 on the identical value), we do not error: falling
		// through to cwd/city keeps cross-city queries working from rig agents
		// whose GC_RIG names a rig this city does not bind. But the discard
		// must not be silent — a stale or typo'd GC_RIG would otherwise
		// redirect a query to a different store than the operator intended with
		// no diagnostic, while the same value via --rig fails loudly. Record it
		// and warn below, naming the store actually answered.
		gcRigDiscarded = gcRig
	}

	target := cityTarget
	if rig, ok, err := bdRigFromCwd(cfg, cityPath); err != nil {
		return execStoreTarget{}, err
	} else if ok {
		// resolveRigForDir already skips unbound rigs, so rig.Path is
		// guaranteed non-empty here.
		target = bdRigScopeTarget(cityPath, rig)
	}

	if gcRigDiscarded != "" {
		fmt.Fprintf(stderr, "gc bd: warning: GC_RIG=%q does not name a bound rig in this city; ignoring it and answering from the %s store instead (the same value via --rig would exit 1)\n", gcRigDiscarded, scopeLabel(target)) //nolint:errcheck // best-effort stderr
	}
	return target, nil
}

// bdScopeDisclosureVerbs are the bd read-only passthrough verbs whose
// resolved store gets announced on stderr (gastownhall/gascity#5170). Scoped
// to reads: a write verb's effect is directly observable (the record it
// touched can be re-read), while a read verb's silence is exactly what makes
// an empty answer indistinguishable from "no matches in the store that was
// asked."
var bdScopeDisclosureVerbs = map[string]bool{
	"list":   true,
	"ready":  true,
	"search": true,
	"show":   true,
}

// scopeLabel renders a store target for operator-facing diagnostics, e.g.
// `city` or `rig "packs"`.
func scopeLabel(t execStoreTarget) string {
	if t.ScopeKind == "rig" && strings.TrimSpace(t.RigName) != "" {
		return fmt.Sprintf("rig %q", t.RigName)
	}
	return t.ScopeKind
}

func bdRigForArg(cfg *config.City, arg string) (config.Rig, bool) {
	if prefix := beadPrefix(cfg, arg); prefix != "" {
		return findRigByPrefix(cfg, prefix)
	}
	return config.Rig{}, false
}

func bdRigFromCwd(cfg *config.City, cityPath string) (config.Rig, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Rig{}, false, nil
	}
	return resolveRigForDir(cfg, cityPath, cwd)
}

func bdRigScopeTarget(cityPath string, rig config.Rig) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, rig.Path),
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}
}

func bdCityScopeTarget(cityPath string, cfg *config.City) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, cityPath),
		ScopeKind: "city",
		Prefix:    config.EffectiveHQPrefix(cfg),
	}
}
