// Package selectorinventory parses bounded, injected fixture inputs for a
// privacy-limited inventory of possible external selector sources.
//
// It deliberately has no operating-system readers. Callers provide filesystem,
// process, clock, identity, and system-manager readers; tests use fixture
// implementations. This package never executes discovered commands and its
// serialized types contain no raw paths, command lines, environments, unit
// contents, identities, or errors.
package selectorinventory

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// CollectorVersion identifies this inventory parser and record contract.
const CollectorVersion = "selector-host-inventory-v1"

// Status is the complete set of outcomes understood by this collector.
type Status string

const (
	StatusAvailable        Status = "available"         // The bounded source read and parse succeeded.
	StatusAbsent           Status = "absent"            // The enumerated source did not exist.
	StatusUnavailable      Status = "unavailable"       // The source could not be read or joined.
	StatusPermissionDenied Status = "permission_denied" // The reader could not access the source.
	StatusParseError       Status = "parse_error"       // The source was malformed or unsafe to parse.
	StatusRaced            Status = "raced"             // The source changed across its capture fence.
	StatusUnsupported      Status = "unsupported"       // The injected reader does not support the source.
)

// CaptureWindow bounds one source read or the complete collection.
type CaptureWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ControllerBinding must be copied from the already sealed controller
// SelectorObservationSnapshot. This package does not read or mutate it.
type ControllerBinding struct {
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Generation     uint64 `json:"generation"`
	Build          string `json:"build"`
}

// SourceRecord reports one allowlisted source's result and capture window.
type SourceRecord struct {
	ID              string        `json:"id"`
	Status          Status        `json:"status"`
	Required        bool          `json:"required"`
	Capture         CaptureWindow `json:"capture"`
	WatermarkSHA256 string        `json:"watermark_sha256,omitempty"`
	IssueCodes      []string      `json:"issue_codes"`
}

// Finding stores only stable identifiers, a keyed path/unit identifier, and
// limited schedule/classification metadata. It never stores discovered text.
type Finding struct {
	SourceID       string `json:"source_id"`
	PathID         string `json:"path_id"`
	ProcessID      string `json:"process_id,omitempty"`
	ScriptID       string `json:"script_id"`
	Classification string `json:"classification"`
	Schedule       string `json:"schedule,omitempty"`
}

// Record is observation evidence only. Status remains unavailable while exact
// controller in-flight identities are unavailable, even if every enumerated
// host source was read successfully.
type Record struct {
	SchemaVersion      int               `json:"schema_version"`
	CollectorVersion   string            `json:"collector_version"`
	Status             Status            `json:"status"`
	IssueCodes         []string          `json:"issue_codes"`
	Controller         ControllerBinding `json:"controller"`
	PseudonymousHostID string            `json:"pseudonymous_host_id,omitempty"`
	PseudonymousBootID string            `json:"pseudonymous_boot_id,omitempty"`
	Capture            CaptureWindow     `json:"capture"`
	Sources            []SourceRecord    `json:"sources"`
	Findings           []Finding         `json:"findings"`
	DigestSHA256       string            `json:"digest_sha256"`
}

// Limits bounds entries and total transient bytes returned by injected readers.
type Limits struct {
	MaxEntries  int
	MaxBytes    int
	MaxCommands int
}

// TreeEntryKind describes a no-follow entry returned by a filesystem reader.
type TreeEntryKind string

const (
	// EntryRegular is a no-follow regular file entry.
	EntryRegular TreeEntryKind = "regular"
	// EntryDirectory is a no-follow directory entry.
	EntryDirectory TreeEntryKind = "directory"
	// EntrySymlink is a symlink that must never be traversed.
	EntrySymlink TreeEntryKind = "symlink"
	// EntryOther represents a filesystem entry this parser does not support.
	EntryOther TreeEntryKind = "other"
)

// TreeEntry holds transient fixture content for one relative source path.
// RelativePath and Data are never copied to a Record; RelativePath is used
// only to derive a keyed PathID.
type TreeEntry struct {
	RelativePath string        `json:"-"`
	Kind         TreeEntryKind `json:"-"`
	Data         []byte        `json:"-"`
	Watermark    string        `json:"-"`
}

// TreeInput is one bounded filesystem snapshot result.
type TreeInput struct {
	Status      Status      `json:"status"`
	RootSymlink bool        `json:"-"`
	Entries     []TreeEntry `json:"-"`
}

// FilesystemReader is injected and addressed only by fixed SourceID values.
// Implementations must return bounded, no-follow fixture/source snapshots.
type FilesystemReader interface {
	// ReadTree returns a bounded no-follow snapshot. It must report StatusRaced
	// if any source watermark changes during the read.
	ReadTree(sourceID string, limits Limits) TreeInput
}

// Process holds transient process metadata; raw fields never enter a Record.
type Process struct {
	PID         uint64   `json:"-"`
	StartTicks  uint64   `json:"-"`
	Executable  string   `json:"-"`
	Arguments   []string `json:"-"`
	Environment []string `json:"-"`
}

// ProcessInput supplies two process-table reads and the boot ID they belong to.
type ProcessInput struct {
	Status Status    `json:"status"`
	BootID string    `json:"-"`
	Before []Process `json:"-"`
	After  []Process `json:"-"`
}

// ProcessReader returns two bounded process-table reads for PID/start-time fencing.
type ProcessReader interface {
	ReadProcesses(limits Limits) ProcessInput
}

// Identity contains transient host and boot IDs that are keyed before output.
type Identity struct {
	HostID string `json:"-"`
	BootID string `json:"-"`
}

// IdentityInput is the result of reading the injected host identity source.
type IdentityInput struct {
	Status   Status   `json:"status"`
	Identity Identity `json:"-"`
}

// IdentityReader provides the host and boot identifiers to pseudonymize.
type IdentityReader interface {
	ReadIdentity() IdentityInput
}

// ManagerUnit contains transient manager metadata and is never serialized.
type ManagerUnit struct {
	Name           string           `json:"-"`
	Kind           string           `json:"-"`
	Schedule       string           `json:"-"`
	Commands       []ManagerCommand `json:"-"`
	Watermark      string           `json:"-"`
	HasEnvironment bool             `json:"-"`
}

// ManagerCommand is one transient, bounded service execution directive.
// Directive and Command are never serialized.
type ManagerCommand struct {
	Directive string `json:"-"`
	Command   string `json:"-"`
}

// ManagerInput carries independently acquired before/after unit snapshots from
// a fixed manager query. The Collector compares every bounded field in both
// snapshots; a reader must not reuse one snapshot or provide only one watermark
// set as a substitute for the two observations.
type ManagerInput struct {
	Status Status        `json:"status"`
	Before []ManagerUnit `json:"-"`
	After  []ManagerUnit `json:"-"`
}

// ManagerReader receives only fixed, read-only query identifiers. It is not a
// command runner and must independently acquire two bounded snapshots around
// its enumeration. Collector verifies that names, kinds, schedules, every
// command directive/value pair, environment-presence flags, and watermarks
// match before using any unit.
type ManagerReader interface {
	ReadManager(queryID string, limits Limits) ManagerInput
}

// Clock supplies capture-window timestamps.
type Clock interface {
	Now() time.Time
}

// Collector composes only injected readers and performs no operating-system I/O.
type Collector struct {
	Filesystem FilesystemReader
	Processes  ProcessReader
	Identity   IdentityReader
	Manager    ManagerReader
	Clock      Clock
	Binding    ControllerBinding
	HMACKey    []byte `json:"-"`
}

// Source IDs identify the fixed inputs accepted by injected readers.
const (
	SourceHostIdentity          = "host.identity"
	SourceControllerSnapshot    = "controller.snapshot"
	SourceInflightIdentities    = "controller.inflight_identities"
	SourceProcessTable          = "process.table"
	SourceSystemCronFile        = "cron.system_file"
	SourceSystemCronDirectory   = "cron.system_directory"
	SourceAllowlistedUserCron   = "cron.user_allowlisted"
	SourceAnacronConfiguration  = "anacron.configuration"
	SourceAnacronSpoolMetadata  = "anacron.spool_metadata"
	SourceSystemdSystemUnits    = "systemd.system_units"
	SourceSystemdUserUnits      = "systemd.user_units"
	SourceSystemdGeneratedUnits = "systemd.generated_units"
	SourceSystemdRuntimeUnits   = "systemd.runtime_units"
	SourceSystemdDropIns        = "systemd.dropins"
	QuerySystemManagerUnits     = "manager.system.units"
	QueryUserManagerUnits       = "manager.user.units"
)

const (
	maxProcessCount        = 8192
	maxManagerUnits        = 4096
	maxTreeEntries         = 512
	maxTreeBytes           = 512 * 1024
	maxSingleFileBytes     = 64 * 1024
	maxProcessArguments    = 256
	maxProcessEnvironment  = 256
	maxProcessStringBytes  = 8192
	maxIdentityBytes       = 4096
	maxHMACKeyBytes        = 4096
	maxAnacronPeriodDays   = 36500
	maxAnacronDelayMins    = 525600
	maxManagerCommands     = 8192
	maxManagerCommandBytes = 8192
)

// Script IDs identify the known WIP scripts, competing work router, and legacy loop.
const (
	ScriptWIPLabelReady      = "wip_label_ready"
	ScriptWIPDispatchOnce    = "wip_dispatch_once"
	ScriptWIPSweepOrphans    = "wip_sweep_orphans"
	ScriptWorkRouter         = "work_router_route_ready"
	ScriptHistoricalDispatch = "historical_wip_dispatch"
)

type sourceSpec struct {
	id       string
	format   string
	required bool
}

var filesystemSources = []sourceSpec{
	{id: SourceSystemCronFile, format: "cron", required: true},
	{id: SourceSystemCronDirectory, format: "cron", required: true},
	{id: SourceAllowlistedUserCron, format: "cron", required: true},
	{id: SourceAnacronConfiguration, format: "anacron", required: true},
	{id: SourceAnacronSpoolMetadata, format: "metadata", required: true},
	{id: SourceSystemdSystemUnits, format: "systemd", required: true},
	{id: SourceSystemdUserUnits, format: "systemd", required: true},
	{id: SourceSystemdGeneratedUnits, format: "systemd", required: true},
	{id: SourceSystemdRuntimeUnits, format: "systemd", required: true},
	{id: SourceSystemdDropIns, format: "systemd", required: true},
}

var scriptBasenames = map[string]string{
	"label_ready.sh":   ScriptWIPLabelReady,
	"dispatch_once.sh": ScriptWIPDispatchOnce,
	"sweep_orphans.sh": ScriptWIPSweepOrphans,
	"route_ready.py":   ScriptWorkRouter,
	"wip_dispatch.sh":  ScriptHistoricalDispatch,
}

// Collect reads only through injected interfaces. An invalid binding, missing
// reader, missing required source, or source failure is retained explicitly.
func (c Collector) Collect() Record {
	if c.Clock == nil {
		return unavailableClockRecord(c.Binding)
	}
	keyValid := len(c.HMACKey) >= 32 && len(c.HMACKey) <= maxHMACKeyBytes
	start := c.now()
	if start.IsZero() {
		return unavailableClockRecord(c.Binding)
	}
	binding := c.Binding
	if !validBinding(binding) {
		binding = ControllerBinding{}
	}
	record := Record{
		SchemaVersion:    1,
		CollectorVersion: CollectorVersion,
		Status:           StatusUnavailable,
		Controller:       binding,
		Capture:          CaptureWindow{Start: start},
		Sources:          make([]SourceRecord, 0, len(filesystemSources)+6),
		Findings:         make([]Finding, 0),
		IssueCodes:       make([]string, 0),
	}
	if !keyValid {
		record.IssueCodes = append(record.IssueCodes, "privacy_key_unavailable")
	}
	if !validBinding(c.Binding) {
		record.IssueCodes = append(record.IssueCodes, "controller_binding_unavailable")
	}

	processBootID := ""
	identitySource := SourceRecord{ID: SourceHostIdentity, Required: true, IssueCodes: []string{}}
	identityStart := c.now()
	if c.Identity == nil {
		identitySource.Status = StatusUnavailable
		identitySource.IssueCodes = append(identitySource.IssueCodes, "identity_reader_unavailable")
	} else {
		input := c.Identity.ReadIdentity()
		identitySource.Status = normalizeStatus(input.Status)
		if identitySource.Status == StatusAvailable {
			if !keyValid || input.Identity.HostID == "" || input.Identity.BootID == "" || len(input.Identity.HostID) > maxIdentityBytes || len(input.Identity.BootID) > maxIdentityBytes {
				identitySource.Status = StatusUnavailable
				identitySource.IssueCodes = append(identitySource.IssueCodes, "identity_value_unavailable")
			} else {
				processBootID = input.Identity.BootID
				record.PseudonymousHostID = keyedID(c.HMACKey, "host", input.Identity.HostID)
				record.PseudonymousBootID = keyedID(c.HMACKey, "boot", input.Identity.BootID)
			}
		}
	}
	identitySource.Capture = c.window(identityStart)
	identitySource.WatermarkSHA256 = keyedID(c.HMACKey, "source:"+identitySource.ID, record.PseudonymousHostID+"\x00"+record.PseudonymousBootID)
	record.Sources = append(record.Sources, identitySource)

	controllerStart := c.now()
	controllerSource := SourceRecord{
		ID: SourceControllerSnapshot, Required: true,
		Status: StatusAvailable, Capture: CaptureWindow{Start: controllerStart},
		IssueCodes: []string{},
	}
	if !validBinding(c.Binding) {
		controllerSource.Status = StatusUnavailable
		controllerSource.IssueCodes = append(controllerSource.IssueCodes, "controller_binding_unavailable")
	}
	controllerSource.Capture = c.window(controllerStart)
	controllerSource.WatermarkSHA256 = keyedID(c.HMACKey, "controller-snapshot", binding.SnapshotSHA256+"\x00"+strconv.FormatUint(binding.Generation, 10)+"\x00"+binding.Build)
	record.Sources = append(record.Sources, controllerSource)

	for _, spec := range filesystemSources {
		record.Sources = append(record.Sources, c.collectTree(spec, &record.Findings, keyValid))
	}
	record.Sources = append(record.Sources, c.collectProcesses(keyValid, processBootID, &record.Findings))
	record.Sources = append(record.Sources, c.collectManager(QuerySystemManagerUnits, &record.Findings, keyValid))
	record.Sources = append(record.Sources, c.collectManager(QueryUserManagerUnits, &record.Findings, keyValid))

	// This source is known to be absent from the current controller surface:
	// an in-flight count does not supply exact identities.
	inflightStart := c.now()
	record.Sources = append(record.Sources, SourceRecord{
		ID: SourceInflightIdentities, Status: StatusUnavailable, Required: true,
		Capture: c.window(inflightStart), IssueCodes: []string{"exact_identity_reader_unavailable"},
	})

	end := c.now()
	record.Capture.End = end
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		record.IssueCodes = append(record.IssueCodes, "capture_clock_raced")
	}
	for i := range record.Sources {
		window := record.Sources[i].Capture
		if window.Start.IsZero() || window.End.IsZero() {
			record.Sources[i].Status = StatusUnavailable
			record.Sources[i].IssueCodes = append(record.Sources[i].IssueCodes, "capture_window_unavailable")
		} else if window.End.Before(window.Start) {
			record.Sources[i].Status = StatusRaced
			record.Sources[i].IssueCodes = append(record.Sources[i].IssueCodes, "capture_clock_raced")
		}
		record.Sources[i].IssueCodes = uniqueSorted(record.Sources[i].IssueCodes)
	}

	sort.Slice(record.Sources, func(i, j int) bool { return record.Sources[i].ID < record.Sources[j].ID })
	sort.Slice(record.Findings, func(i, j int) bool { return findingLess(record.Findings[i], record.Findings[j]) })
	for _, source := range record.Sources {
		if source.Required && source.Status != StatusAvailable {
			record.IssueCodes = append(record.IssueCodes, "required_source_"+source.ID+"_"+string(source.Status))
		}
		record.IssueCodes = append(record.IssueCodes, source.IssueCodes...)
	}
	record.IssueCodes = uniqueSorted(record.IssueCodes)
	// Status is intentionally unavailable even if source collection succeeded:
	// this slice has no exact in-flight identity registry and grants no authority.
	record.Status = StatusUnavailable
	record.DigestSHA256 = recordDigest(record)
	return record
}

func unavailableClockRecord(binding ControllerBinding) Record {
	if !validBinding(binding) {
		binding = ControllerBinding{}
	}
	record := Record{
		SchemaVersion:    1,
		CollectorVersion: CollectorVersion,
		Status:           StatusUnavailable,
		IssueCodes:       []string{"capture_clock_unavailable"},
		Controller:       binding,
		Sources:          make([]SourceRecord, 0, len(filesystemSources)+6),
		Findings:         []Finding{},
	}
	add := func(id string) {
		record.Sources = append(record.Sources, SourceRecord{
			ID: id, Status: StatusUnavailable, Required: true,
			IssueCodes: []string{"capture_clock_unavailable"},
		})
	}
	add(SourceHostIdentity)
	add(SourceControllerSnapshot)
	for _, spec := range filesystemSources {
		add(spec.id)
	}
	add(SourceProcessTable)
	add(QuerySystemManagerUnits)
	add(QueryUserManagerUnits)
	add(SourceInflightIdentities)
	sort.Slice(record.Sources, func(i, j int) bool { return record.Sources[i].ID < record.Sources[j].ID })
	record.DigestSHA256 = recordDigest(record)
	return record
}

func (c Collector) collectTree(spec sourceSpec, findings *[]Finding, keyValid bool) SourceRecord {
	start := c.now()
	source := SourceRecord{ID: spec.id, Required: spec.required, IssueCodes: []string{}}
	if c.Filesystem == nil {
		source.Status = StatusUnavailable
		source.IssueCodes = append(source.IssueCodes, "filesystem_reader_unavailable")
		source.Capture = c.window(start)
		return source
	}
	input := c.Filesystem.ReadTree(spec.id, Limits{MaxEntries: maxTreeEntries, MaxBytes: maxTreeBytes})
	source.Status = normalizeStatus(input.Status)
	source.Capture = c.window(start)
	if source.Status != StatusAvailable {
		return source
	}
	if input.RootSymlink {
		source.Status = StatusParseError
		source.IssueCodes = append(source.IssueCodes, "symlink_rejected")
		return source
	}
	if !keyValid {
		source.Status = StatusUnavailable
		source.IssueCodes = append(source.IssueCodes, "privacy_key_unavailable")
		return source
	}
	if len(input.Entries) > maxTreeEntries {
		source.Status = StatusParseError
		source.IssueCodes = append(source.IssueCodes, "entry_limit_exceeded")
		return source
	}
	entries := make([]TreeEntry, 0, len(input.Entries))
	for _, entry := range input.Entries {
		if len(entry.RelativePath) > 4096 || len(entry.Watermark) > 4096 {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "entry_metadata_limit_exceeded")
			continue
		}
		if len(entry.Data) > maxSingleFileBytes {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "file_limit_exceeded")
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return treeEntryLess(entries[i], entries[j]) })
	seen := make(map[string]struct{}, len(entries))
	entryCounts := make(map[string]int, len(entries))
	for _, entry := range entries {
		entryCounts[entry.RelativePath]++
	}
	blockedPrefixes := make([]string, 0)
	totalBytes := 0
	var watermark strings.Builder
	timerStems := make(map[string]bool)
	if spec.format == "systemd" {
		for _, entry := range entries {
			if entry.Kind == EntryRegular && strings.HasSuffix(entry.RelativePath, ".timer") && hasSystemdCalendar(entry.Data) {
				timerStems[unitStem(entry.RelativePath)] = true
			}
		}
	}
	for _, entry := range entries {
		if !safeRelativePath(entry.RelativePath) {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "unsafe_relative_path")
			continue
		}
		blocked := false
		for _, prefix := range blockedPrefixes {
			if entry.RelativePath == prefix || strings.HasPrefix(entry.RelativePath, prefix+"/") {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		if entryCounts[entry.RelativePath] > 1 {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "duplicate_entry")
			continue
		}
		if _, ok := seen[entry.RelativePath]; ok {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "duplicate_entry")
			continue
		}
		seen[entry.RelativePath] = struct{}{}
		totalBytes += len(entry.RelativePath) + len(entry.Watermark) + len(entry.Data)
		if totalBytes > maxTreeBytes {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "byte_limit_exceeded")
			break
		}
		watermark.WriteString(entry.RelativePath)
		watermark.WriteByte(0)
		watermark.WriteString(string(entry.Kind))
		watermark.WriteByte(0)
		watermark.WriteString(entry.Watermark)
		watermark.WriteByte(0)
		watermark.WriteString(keyedID(c.HMACKey, "file-content", string(entry.Data)))
		watermark.WriteByte(0)
		if entry.Kind == EntrySymlink {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "symlink_rejected")
			blockedPrefixes = append(blockedPrefixes, entry.RelativePath)
			continue
		}
		if entry.Kind == EntryDirectory {
			continue
		}
		if entry.Kind != EntryRegular {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "unsupported_entry_type")
			continue
		}
		pathID := keyedID(c.HMACKey, "path:"+spec.id, entry.RelativePath)
		parsed, codes := parseSourceData(spec.format, spec.id, pathID, entry.RelativePath, entry.Data)
		if spec.format == "systemd" && strings.HasSuffix(entry.RelativePath, ".service") && timerStems[unitStem(entry.RelativePath)] {
			for i := range parsed {
				parsed[i].Schedule = "systemd_timer"
			}
		}
		*findings = append(*findings, parsed...)
		if len(codes) > 0 {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, codes...)
		}
	}
	source.WatermarkSHA256 = keyedID(c.HMACKey, "tree:"+spec.id, watermark.String())
	source.IssueCodes = uniqueSorted(source.IssueCodes)
	return source
}

func (c Collector) collectProcesses(keyValid bool, expectedBootID string, findings *[]Finding) SourceRecord {
	start := c.now()
	source := SourceRecord{ID: SourceProcessTable, Required: true, IssueCodes: []string{}}
	if c.Processes == nil {
		source.Status = StatusUnavailable
		source.IssueCodes = append(source.IssueCodes, "process_reader_unavailable")
		source.Capture = c.window(start)
		return source
	}
	input := c.Processes.ReadProcesses(Limits{MaxEntries: maxProcessCount, MaxBytes: maxTreeBytes})
	source.Status = normalizeStatus(input.Status)
	source.Capture = c.window(start)
	if source.Status != StatusAvailable {
		return source
	}
	if expectedBootID == "" || input.BootID == "" || len(input.BootID) > maxIdentityBytes {
		source.Status = StatusUnavailable
		source.IssueCodes = append(source.IssueCodes, "process_boot_identity_unavailable")
		return source
	}
	if input.BootID != expectedBootID {
		source.Status = StatusRaced
		source.IssueCodes = append(source.IssueCodes, "process_boot_identity_changed")
		return source
	}
	if !keyValid {
		source.Status = StatusUnavailable
		source.IssueCodes = append(source.IssueCodes, "privacy_key_unavailable")
		return source
	}
	if len(input.Before) > maxProcessCount || len(input.After) > maxProcessCount {
		source.Status = StatusParseError
		source.IssueCodes = append(source.IssueCodes, "process_limit_exceeded")
		return source
	}
	if !processInputWithinLimits(input.Before) || !processInputWithinLimits(input.After) {
		source.Status = StatusParseError
		source.IssueCodes = append(source.IssueCodes, "process_metadata_limit_exceeded")
		return source
	}
	before, validBefore := processMap(input.Before)
	after, validAfter := processMap(input.After)
	if !validBefore || !validAfter || !sameProcessFence(before, after) {
		source.Status = StatusRaced
		source.IssueCodes = append(source.IssueCodes, "pid_identity_changed")
		return source
	}
	processes := make([]Process, 0, len(after))
	for _, process := range after {
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool {
		if processes[i].PID != processes[j].PID {
			return processes[i].PID < processes[j].PID
		}
		return processes[i].StartTicks < processes[j].StartTicks
	})
	for _, process := range processes {
		if hasShellIndirection(process.Executable) {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "process_command_text_unsupported")
			return source
		}
		for _, argument := range process.Arguments {
			if hasShellIndirection(argument) {
				source.Status = StatusParseError
				source.IssueCodes = append(source.IssueCodes, "process_command_text_unsupported")
				return source
			}
		}
	}
	var watermark strings.Builder
	watermark.WriteString(keyedID(c.HMACKey, "process-boot-id", input.BootID))
	watermark.WriteByte(0)
	for _, process := range processes {
		watermark.WriteString(strconv.FormatUint(process.PID, 10))
		watermark.WriteByte(0)
		watermark.WriteString(strconv.FormatUint(process.StartTicks, 10))
		watermark.WriteByte(0)
		watermark.WriteString(keyedID(c.HMACKey, "process-executable", process.Executable))
		watermark.WriteByte(0)
		for _, argument := range process.Arguments {
			watermark.WriteString(keyedID(c.HMACKey, "process-argument", argument))
			watermark.WriteByte(0)
		}
		processText := make([]string, 0, len(process.Arguments)+1)
		processText = append(processText, process.Executable)
		processText = append(processText, process.Arguments...)
		for _, scriptID := range matchingScripts(processText...) {
			*findings = append(*findings, Finding{
				SourceID:  SourceProcessTable,
				PathID:    keyedID(c.HMACKey, "process-path", process.Executable+"\x00"+strings.Join(process.Arguments, "\x00")),
				ProcessID: keyedID(c.HMACKey, "process-id", input.BootID+"\x00"+strconv.FormatUint(process.PID, 10)+"\x00"+strconv.FormatUint(process.StartTicks, 10)),
				ScriptID:  scriptID, Classification: "process",
			})
		}
	}
	source.WatermarkSHA256 = keyedID(c.HMACKey, "process-table", watermark.String())
	return source
}

func (c Collector) collectManager(queryID string, findings *[]Finding, keyValid bool) SourceRecord {
	start := c.now()
	source := SourceRecord{ID: queryID, Required: true, IssueCodes: []string{}}
	if queryID != QuerySystemManagerUnits && queryID != QueryUserManagerUnits {
		source.Status = StatusUnsupported
		source.IssueCodes = append(source.IssueCodes, "manager_query_not_allowlisted")
		source.Capture = c.window(start)
		return source
	}
	if c.Manager == nil {
		source.Status = StatusUnsupported
		source.IssueCodes = append(source.IssueCodes, "manager_reader_unavailable")
		source.Capture = c.window(start)
		return source
	}
	input := c.Manager.ReadManager(queryID, Limits{
		MaxEntries: maxManagerUnits, MaxBytes: maxTreeBytes, MaxCommands: maxManagerCommands / 2,
	})
	source.Status = normalizeStatus(input.Status)
	source.Capture = c.window(start)
	if source.Status != StatusAvailable {
		return source
	}
	if !keyValid {
		source.Status = StatusUnavailable
		source.IssueCodes = append(source.IssueCodes, "privacy_key_unavailable")
		return source
	}
	if len(input.Before) > maxManagerUnits || len(input.After) > maxManagerUnits {
		source.Status = StatusParseError
		source.IssueCodes = append(source.IssueCodes, "manager_limit_exceeded")
		return source
	}
	if !managerInputWithinLimits(input.Before, input.After) {
		source.Status = StatusParseError
		source.IssueCodes = append(source.IssueCodes, "manager_metadata_limit_exceeded")
		return source
	}
	before := append([]ManagerUnit(nil), input.Before...)
	after := append([]ManagerUnit(nil), input.After...)
	sort.Slice(before, func(i, j int) bool { return managerUnitLess(before[i], before[j]) })
	sort.Slice(after, func(i, j int) bool { return managerUnitLess(after[i], after[j]) })
	if !sameManagerSnapshot(before, after) {
		source.Status = StatusRaced
		source.IssueCodes = append(source.IssueCodes, "manager_snapshot_changed")
		return source
	}
	unsafeUnits := make(map[string]bool, len(after))
	for _, unit := range after {
		if unit.Name == "" || (unit.Kind != "service" && unit.Kind != "timer") {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "manager_record_invalid")
			unsafeUnits[unit.Name] = true
		}
	}
	var watermark strings.Builder
	timers := make(map[string]string)
	for _, unit := range after {
		watermark.WriteString(keyedID(c.HMACKey, "manager-name", unit.Name))
		watermark.WriteByte(0)
		watermark.WriteString(keyedID(c.HMACKey, "manager-kind", unit.Kind))
		watermark.WriteByte(0)
		watermark.WriteString(keyedID(c.HMACKey, "manager-schedule", unit.Schedule))
		watermark.WriteByte(0)
		watermark.WriteString(strconv.Itoa(len(unit.Commands)))
		watermark.WriteByte(0)
		for _, command := range unit.Commands {
			watermark.WriteString(keyedID(c.HMACKey, "manager-directive", command.Directive))
			watermark.WriteByte(0)
			watermark.WriteString(keyedID(c.HMACKey, "manager-command", command.Command))
			watermark.WriteByte(0)
		}
		watermark.WriteString(keyedID(c.HMACKey, "manager-watermark", unit.Watermark))
		watermark.WriteByte(0)
		watermark.WriteString(strconv.FormatBool(unit.HasEnvironment))
		watermark.WriteByte(0)
	}
	seenNames := make(map[string]struct{}, len(after))
	for _, unit := range after {
		if unit.Name == "" || (unit.Kind != "service" && unit.Kind != "timer") {
			continue
		}
		if _, exists := seenNames[unit.Name]; exists {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "manager_record_duplicate")
			unsafeUnits[unit.Name] = true
			continue
		}
		seenNames[unit.Name] = struct{}{}
		if unit.HasEnvironment {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "systemd_indirection_unsupported")
			unsafeUnits[unit.Name] = true
		}
		if unit.Kind != "service" && len(unit.Commands) > 0 {
			source.Status = StatusParseError
			source.IssueCodes = append(source.IssueCodes, "manager_command_kind_unsupported")
			unsafeUnits[unit.Name] = true
		}
		for _, command := range unit.Commands {
			if !supportedSystemdExecDirective(command.Directive) {
				source.Status = StatusParseError
				source.IssueCodes = append(source.IssueCodes, "manager_exec_directive_unsupported")
				unsafeUnits[unit.Name] = true
				continue
			}
			if command.Command == "" {
				source.Status = StatusParseError
				source.IssueCodes = append(source.IssueCodes, "manager_command_invalid")
				unsafeUnits[unit.Name] = true
				continue
			}
			if hasSystemdCommandIndirection(command.Command) {
				source.Status = StatusParseError
				source.IssueCodes = append(source.IssueCodes, "systemd_indirection_unsupported")
				unsafeUnits[unit.Name] = true
			}
		}
		if unit.Kind == "timer" && unit.Schedule != "" {
			timers[unitStem(unit.Name)] = "systemd_timer"
		}
	}
	for _, unit := range after {
		if unit.Name == "" || unit.Kind != "service" || unit.HasEnvironment || unsafeUnits[unit.Name] {
			continue
		}
		for _, command := range unit.Commands {
			for _, scriptID := range matchingScripts(command.Command) {
				schedule := timers[unitStem(unit.Name)]
				*findings = append(*findings, Finding{
					SourceID: queryID,
					PathID:   keyedID(c.HMACKey, "manager-unit:"+queryID, unit.Name),
					ScriptID: scriptID, Classification: "systemd_manager",
					Schedule: schedule,
				})
			}
		}
	}
	source.WatermarkSHA256 = keyedID(c.HMACKey, "manager:"+queryID, watermark.String())
	source.IssueCodes = uniqueSorted(source.IssueCodes)
	return source
}

func (c Collector) now() time.Time {
	return c.Clock.Now().UTC()
}

func (c Collector) window(start time.Time) CaptureWindow {
	return CaptureWindow{Start: start, End: c.now()}
}

func parseSourceData(format, sourceID, pathID, relativePath string, data []byte) ([]Finding, []string) {
	switch format {
	case "cron":
		return parseCron(sourceID, pathID, data)
	case "anacron":
		return parseAnacron(sourceID, pathID, data)
	case "systemd":
		return parseSystemdSingle(sourceID, pathID, relativePath, data)
	case "metadata":
		return nil, nil
	default:
		return nil, []string{"source_format_unsupported"}
	}
}

func parseCron(sourceID, pathID string, data []byte) ([]Finding, []string) {
	if !utf8.Valid(data) {
		return nil, []string{"invalid_utf8"}
	}
	var findings []Finding
	var issues []string
	for _, rawLine := range strings.Split(string(data), "\n") {
		if len(rawLine) > 4096 {
			issues = append(issues, "line_limit_exceeded")
			continue
		}
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if isCronEnvironment(line) {
			issues = append(issues, "cron_environment_indirection_unsupported")
			continue
		}
		fields := strings.Fields(line)
		var scheduleClass, command string
		if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
			if len(fields) < 2 || !validCronAlias(fields[0]) {
				issues = append(issues, "cron_entry_malformed")
				continue
			}
			scheduleClass = "cron_calendar"
			if fields[0] == "@reboot" {
				scheduleClass = "cron_event"
			}
			command = strings.Join(fields[1:], " ")
		} else {
			if len(fields) < 6 || !validCronFields(fields[:5]) {
				issues = append(issues, "cron_entry_malformed")
				continue
			}
			scheduleClass, command = "cron_calendar", strings.Join(fields[5:], " ")
		}
		if hasShellIndirection(command) {
			issues = append(issues, "cron_command_indirection_unsupported")
			continue
		}
		for _, scriptID := range matchingScripts(command) {
			findings = append(findings, Finding{SourceID: sourceID, PathID: pathID, ScriptID: scriptID, Classification: "cron", Schedule: scheduleClass})
		}
	}
	return findings, uniqueSorted(issues)
}

func parseAnacron(sourceID, pathID string, data []byte) ([]Finding, []string) {
	if !utf8.Valid(data) {
		return nil, []string{"invalid_utf8"}
	}
	var findings []Finding
	var issues []string
	for _, rawLine := range strings.Split(string(data), "\n") {
		if len(rawLine) > 4096 {
			issues = append(issues, "line_limit_exceeded")
			continue
		}
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			issues = append(issues, "anacron_entry_malformed")
			continue
		}
		if _, ok := parseBoundedDecimal(fields[0], 1, maxAnacronPeriodDays); !ok {
			issues = append(issues, "anacron_entry_malformed")
			continue
		}
		if _, ok := parseBoundedDecimal(fields[1], 0, maxAnacronDelayMins); !ok {
			issues = append(issues, "anacron_entry_malformed")
			continue
		}
		command := strings.Join(fields[3:], " ")
		if hasShellIndirection(command) {
			issues = append(issues, "anacron_command_indirection_unsupported")
			continue
		}
		for _, scriptID := range matchingScripts(command) {
			findings = append(findings, Finding{SourceID: sourceID, PathID: pathID, ScriptID: scriptID, Classification: "anacron", Schedule: "anacron_periodic"})
		}
	}
	return findings, uniqueSorted(issues)
}

func parseSystemdSingle(sourceID, pathID, relativePath string, data []byte) ([]Finding, []string) {
	if !utf8.Valid(data) {
		return nil, []string{"invalid_utf8"}
	}
	section := ""
	execValuesByKey := make(map[string][]string)
	var schedules []string
	var issues []string
	for _, rawLine := range strings.Split(string(data), "\n") {
		if len(rawLine) > 4096 {
			issues = append(issues, "line_limit_exceeded")
			continue
		}
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			issues = append(issues, "systemd_continuation_unsupported")
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && len(line) > 2 {
			section = line[1 : len(line)-1]
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			issues = append(issues, "systemd_entry_malformed")
			continue
		}
		switch {
		case section == "Service" && (key == "Environment" || key == "EnvironmentFile" || key == "PassEnvironment" || key == "UnsetEnvironment"):
			issues = append(issues, "systemd_indirection_unsupported")
		case section == "Service" && strings.HasPrefix(strings.ToLower(key), "exec"):
			if !supportedSystemdExecDirective(key) {
				issues = append(issues, "systemd_exec_directive_unsupported")
				continue
			}
			value = strings.TrimSpace(value)
			if value == "" {
				delete(execValuesByKey, key)
				continue
			}
			if hasSystemdCommandIndirection(value) {
				issues = append(issues, "systemd_indirection_unsupported")
				delete(execValuesByKey, key)
				continue
			}
			execValuesByKey[key] = append(execValuesByKey[key], value)
		case section == "Timer" && key == "OnCalendar":
			schedules = append(schedules, strings.TrimSpace(value))
		}
	}
	var findings []Finding
	var schedule string
	if len(schedules) > 0 {
		schedule = "systemd_timer"
	}
	for _, directive := range []string{"ExecCondition", "ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost"} {
		for _, execValue := range execValuesByKey[directive] {
			for _, scriptID := range matchingScripts(execValue) {
				findings = append(findings, Finding{SourceID: sourceID, PathID: pathID, ScriptID: scriptID, Classification: "systemd_unit", Schedule: schedule})
			}
		}
	}
	_ = relativePath // The keyed PathID is supplied by the caller; raw names stay transient.
	return findings, uniqueSorted(issues)
}

func hasSystemdCalendar(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && len(line) > 2 {
			section = line[1 : len(line)-1]
			continue
		}
		if section == "Timer" {
			key, value, ok := strings.Cut(line, "=")
			if ok && key == "OnCalendar" && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func hasSystemdCommandIndirection(value string) bool {
	return strings.ContainsAny(value, "$%'\"\\")
}

func supportedSystemdExecDirective(key string) bool {
	switch key {
	case "ExecCondition", "ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost":
		return true
	default:
		return false
	}
}

// hasShellIndirection rejects shell syntax that can resolve a different
// command at runtime. Quoting is also unsupported because it can split a
// known basename across tokens. Cron treats percent specially, so it is
// included alongside expansions, substitutions, globbing, and shell operators.
func hasShellIndirection(value string) bool {
	return strings.ContainsAny(value, "$`~*?[]{}\\%;&|<>()'\"")
}

func matchingScripts(texts ...string) []string {
	found := make(map[string]struct{})
	for _, text := range texts {
		for basename, scriptID := range scriptBasenames {
			if strings.Contains(text, basename) {
				found[scriptID] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(found))
	for scriptID := range found {
		result = append(result, scriptID)
	}
	sort.Strings(result)
	return result
}

func validCronAlias(value string) bool {
	switch value {
	case "@reboot", "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
		return true
	default:
		return false
	}
}

func validCronFields(fields []string) bool {
	if len(fields) != 5 {
		return false
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, field := range fields {
		if !validCronField(field, ranges[i][0], ranges[i][1]) {
			return false
		}
	}
	return true
}

func validCronField(field string, minimum, maximum int) bool {
	if field == "" {
		return false
	}
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return false
		}
		base, stepText, hasStep := strings.Cut(item, "/")
		if hasStep {
			step, ok := parseCronInteger(stepText)
			if !ok || step < 1 || step > maximum-minimum+1 {
				return false
			}
		}
		if strings.Contains(base, "*") {
			if base != "*" {
				return false
			}
			continue
		}
		startText, endText, isRange := strings.Cut(base, "-")
		start, ok := parseCronInteger(startText)
		if !ok || start < minimum || start > maximum {
			return false
		}
		if isRange {
			end, ok := parseCronInteger(endText)
			if !ok || end < minimum || end > maximum || start > end {
				return false
			}
		}
	}
	return true
}

func parseCronInteger(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

func parseBoundedDecimal(value string, minimum, maximum int) (int, bool) {
	if len(value) == 0 || len(value) > len(strconv.Itoa(maximum)) {
		return 0, false
	}
	parsed, ok := parseCronInteger(value)
	if !ok || parsed < minimum || parsed > maximum {
		return 0, false
	}
	return parsed, true
}

func isCronEnvironment(line string) bool {
	key, _, ok := strings.Cut(line, "=")
	if !ok || key == "" {
		return false
	}
	for i, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func processMap(processes []Process) (map[uint64]Process, bool) {
	result := make(map[uint64]Process, len(processes))
	for _, process := range processes {
		if process.PID == 0 || process.StartTicks == 0 {
			return nil, false
		}
		if _, exists := result[process.PID]; exists {
			return nil, false
		}
		result[process.PID] = process
	}
	return result, true
}

func treeEntryLess(a, b TreeEntry) bool {
	if a.RelativePath != b.RelativePath {
		return a.RelativePath < b.RelativePath
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Watermark != b.Watermark {
		return a.Watermark < b.Watermark
	}
	return bytes.Compare(a.Data, b.Data) < 0
}

func managerUnitLess(a, b ManagerUnit) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Schedule != b.Schedule {
		return a.Schedule < b.Schedule
	}
	if !sameManagerCommands(a.Commands, b.Commands) {
		return managerCommandsLess(a.Commands, b.Commands)
	}
	if a.Watermark != b.Watermark {
		return a.Watermark < b.Watermark
	}
	return !a.HasEnvironment && b.HasEnvironment
}

func managerCommandsLess(a, b []ManagerCommand) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i].Directive != b[i].Directive {
			return a[i].Directive < b[i].Directive
		}
		if a[i].Command != b[i].Command {
			return a[i].Command < b[i].Command
		}
	}
	return len(a) < len(b)
}

func managerInputWithinLimits(before, after []ManagerUnit) bool {
	totalBytes := 0
	commandCount := 0
	for _, units := range [][]ManagerUnit{before, after} {
		snapshotCommandCount := 0
		for _, unit := range units {
			if len(unit.Name) > 4096 || len(unit.Schedule) > 4096 || len(unit.Watermark) > 4096 {
				return false
			}
			totalBytes += len(unit.Name) + len(unit.Schedule) + len(unit.Watermark)
			if totalBytes > maxTreeBytes {
				return false
			}
			for _, command := range unit.Commands {
				commandCount++
				snapshotCommandCount++
				if commandCount > maxManagerCommands || snapshotCommandCount > maxManagerCommands/2 || len(command.Directive) > 64 || len(command.Command) > maxManagerCommandBytes {
					return false
				}
				totalBytes += len(command.Directive) + len(command.Command)
				if totalBytes > maxTreeBytes {
					return false
				}
			}
		}
	}
	return true
}

func sameManagerCommands(a, b []ManagerCommand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Directive != b[i].Directive || a[i].Command != b[i].Command {
			return false
		}
	}
	return true
}

func sameManagerSnapshot(before, after []ManagerUnit) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		a, b := before[i], after[i]
		if a.Name != b.Name || a.Kind != b.Kind || a.Schedule != b.Schedule || !sameManagerCommands(a.Commands, b.Commands) || a.Watermark != b.Watermark || a.HasEnvironment != b.HasEnvironment {
			return false
		}
	}
	return true
}

func processInputWithinLimits(processes []Process) bool {
	totalBytes := 0
	for _, process := range processes {
		if len(process.Executable) > maxProcessStringBytes || len(process.Arguments) > maxProcessArguments || len(process.Environment) > maxProcessEnvironment {
			return false
		}
		totalBytes += len(process.Executable)
		for _, argument := range process.Arguments {
			if len(argument) > maxProcessStringBytes {
				return false
			}
			totalBytes += len(argument)
		}
		// Environment values are never parsed, hashed, or serialized. The
		// injected reader still has to bound them before returning the fixture.
		for _, variable := range process.Environment {
			if len(variable) > maxProcessStringBytes {
				return false
			}
			totalBytes += len(variable)
		}
		if totalBytes > maxTreeBytes {
			return false
		}
	}
	return true
}

func sameProcessFence(before, after map[uint64]Process) bool {
	if len(before) != len(after) {
		return false
	}
	for pid, first := range before {
		last, ok := after[pid]
		if !ok || first.StartTicks != last.StartTicks || first.Executable != last.Executable || !sameStrings(first.Arguments, last.Arguments) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func unitStem(name string) string {
	if strings.HasSuffix(name, ".service") || strings.HasSuffix(name, ".timer") {
		return name[:strings.LastIndexByte(name, '.')]
	}
	return name
}

func safeRelativePath(value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func validBinding(binding ControllerBinding) bool {
	if len(binding.SnapshotSHA256) != 64 || binding.Generation == 0 || binding.Build == "" || len(binding.Build) > 128 {
		return false
	}
	if _, err := hex.DecodeString(binding.SnapshotSHA256); err != nil || strings.ToLower(binding.SnapshotSHA256) != binding.SnapshotSHA256 {
		return false
	}
	for _, r := range binding.Build {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+-", r) {
			continue
		}
		return false
	}
	return true
}

func normalizeStatus(status Status) Status {
	switch status {
	case StatusAvailable, StatusAbsent, StatusUnavailable, StatusPermissionDenied, StatusParseError, StatusRaced, StatusUnsupported:
		return status
	default:
		return StatusUnavailable
	}
}

func keyedID(key []byte, context, value string) string {
	if len(key) < 32 || len(key) > maxHMACKeyBytes {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(context))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func recordDigest(record Record) string {
	record.DigestSHA256 = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func findingLess(a, b Finding) bool {
	if a.SourceID != b.SourceID {
		return a.SourceID < b.SourceID
	}
	if a.PathID != b.PathID {
		return a.PathID < b.PathID
	}
	if a.ProcessID != b.ProcessID {
		return a.ProcessID < b.ProcessID
	}
	if a.ScriptID != b.ScriptID {
		return a.ScriptID < b.ScriptID
	}
	if a.Classification != b.Classification {
		return a.Classification < b.Classification
	}
	return a.Schedule < b.Schedule
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if value == "" {
			continue
		}
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

// JoinStatus checks that this record is bound to the caller's exact controller
// snapshot. A matching join remains unavailable until exact in-flight identities
// exist; matching digests never imply absence or retirement authority.
type JoinResult struct {
	Status    Status `json:"status"`
	IssueCode string `json:"issue_code,omitempty"`
}

// JoinStatus verifies the controller snapshot and record digest, then reports
// unavailable until exact in-flight identities can be joined.
func JoinStatus(record Record, expected ControllerBinding) JoinResult {
	if !validBinding(expected) || record.Controller != expected {
		return JoinResult{Status: StatusUnavailable, IssueCode: "controller_snapshot_join_mismatch"}
	}
	if record.DigestSHA256 == "" || record.DigestSHA256 != recordDigest(record) {
		return JoinResult{Status: StatusUnavailable, IssueCode: "inventory_digest_mismatch"}
	}
	if record.SchemaVersion != 1 || record.CollectorVersion != CollectorVersion || record.Status != StatusUnavailable {
		return JoinResult{Status: StatusUnavailable, IssueCode: "inventory_schema_unsupported"}
	}
	expectedSources := make(map[string]struct{}, len(filesystemSources)+6)
	for _, id := range []string{
		SourceHostIdentity, SourceControllerSnapshot, SourceProcessTable,
		QuerySystemManagerUnits, QueryUserManagerUnits, SourceInflightIdentities,
	} {
		expectedSources[id] = struct{}{}
	}
	for _, spec := range filesystemSources {
		expectedSources[spec.id] = struct{}{}
	}
	seenSources := make(map[string]struct{}, len(expectedSources))
	for _, source := range record.Sources {
		if _, expected := expectedSources[source.ID]; !expected {
			return JoinResult{Status: StatusUnavailable, IssueCode: "required_source_unavailable"}
		}
		if _, duplicate := seenSources[source.ID]; duplicate || !source.Required {
			return JoinResult{Status: StatusUnavailable, IssueCode: "required_source_unavailable"}
		}
		seenSources[source.ID] = struct{}{}
		if source.ID == SourceInflightIdentities {
			if source.Status != StatusAvailable {
				return JoinResult{Status: StatusUnavailable, IssueCode: "exact_identity_reader_unavailable"}
			}
			continue
		}
		if source.Status != StatusAvailable {
			return JoinResult{Status: StatusUnavailable, IssueCode: "required_source_unavailable"}
		}
	}
	if len(seenSources) != len(expectedSources) {
		return JoinResult{Status: StatusUnavailable, IssueCode: "required_source_unavailable"}
	}
	for _, source := range record.Sources {
		if source.Required && source.Status != StatusAvailable {
			return JoinResult{Status: StatusUnavailable, IssueCode: "required_source_unavailable"}
		}
	}
	return JoinResult{Status: StatusUnavailable, IssueCode: "exact_identity_reader_unavailable"}
}
