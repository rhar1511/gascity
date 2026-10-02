package selectorinventory

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fixtureFilesystem struct {
	inputs map[string]TreeInput
	calls  []string
	limits map[string]Limits
}

func (f *fixtureFilesystem) ReadTree(sourceID string, limits Limits) TreeInput {
	f.calls = append(f.calls, sourceID)
	f.limits[sourceID] = limits
	if input, ok := f.inputs[sourceID]; ok {
		return input
	}
	return TreeInput{Status: StatusAbsent}
}

type fixtureProcesses struct {
	input  ProcessInput
	limits Limits
}

func (f *fixtureProcesses) ReadProcesses(limits Limits) ProcessInput {
	f.limits = limits
	return f.input
}

type fixtureIdentity struct{ input IdentityInput }

func (f fixtureIdentity) ReadIdentity() IdentityInput { return f.input }

type fixtureManager struct {
	inputs     map[string]ManagerInput
	queries    []string
	limits     map[string]Limits
	unexpected bool
}

func (f *fixtureManager) ReadManager(queryID string, limits Limits) ManagerInput {
	if queryID != QuerySystemManagerUnits && queryID != QueryUserManagerUnits {
		f.unexpected = true
		return ManagerInput{Status: StatusUnsupported}
	}
	f.queries = append(f.queries, queryID)
	f.limits[queryID] = limits
	return f.inputs[queryID]
}

type fixtureClock struct{ at time.Time }

func (f fixtureClock) Now() time.Time { return f.at }

func testBinding() ControllerBinding {
	return ControllerBinding{SnapshotSHA256: strings.Repeat("a", 64), Generation: 19, Build: "build-2026.09.28"}
}

func testCollector() (Collector, *fixtureFilesystem, *fixtureManager) {
	inputs := make(map[string]TreeInput, len(filesystemSources))
	for _, source := range filesystemSources {
		inputs[source.id] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{}}
	}
	fs := &fixtureFilesystem{inputs: inputs, limits: make(map[string]Limits)}
	manager := &fixtureManager{inputs: map[string]ManagerInput{
		QuerySystemManagerUnits: {Status: StatusAvailable, Before: []ManagerUnit{}, After: []ManagerUnit{}},
		QueryUserManagerUnits:   {Status: StatusAvailable, Before: []ManagerUnit{}, After: []ManagerUnit{}},
	}, limits: make(map[string]Limits)}
	collector := Collector{
		Filesystem: fs,
		Processes:  &fixtureProcesses{input: ProcessInput{Status: StatusAvailable, BootID: "boot-fixture", Before: []Process{}, After: []Process{}}},
		Identity:   fixtureIdentity{input: IdentityInput{Status: StatusAvailable, Identity: Identity{HostID: "host-fixture", BootID: "boot-fixture"}}},
		Manager:    manager,
		Clock:      fixtureClock{at: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)},
		Binding:    testBinding(),
		HMACKey:    []byte("fixture-only-key-that-is-at-least-32-bytes-long"),
	}
	return collector, fs, manager
}

func TestCollectFindsKnownScriptsAndDoesNotSerializeFixtureSecrets(t *testing.T) {
	collector, fs, manager := testCollector()
	fs.inputs["cron.system_file"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "secret_user_canary/cron.tab", Kind: EntryRegular, Watermark: "file-watermark-canary",
		Data: []byte("* * * * * /private/roles/wip-dispatch/label_ready.sh --token=COMMAND_CANARY\n" +
			"*/5 * * * * /private/roles/wip-dispatch/dispatch_once.sh\n" +
			"@hourly /private/roles/wip-dispatch/sweep_orphans.sh\n" +
			"SECRET_ENV_CANARY=PRIVATE_ENV_VALUE\n"),
	}}}
	fs.inputs["anacron.configuration"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "anacrontab", Kind: EntryRegular,
		Data: []byte("1 5 private_job /private/roles/wip-dispatch/sweep_orphans.sh --password=ANACRON_COMMAND_CANARY\n"),
	}}}
	fs.inputs["systemd.system_units"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{
		{RelativePath: "private_user_canary/router.service", Kind: EntryRegular, Data: []byte("[Service]\nEnvironment=TOKEN=UNIT_CONTENT_CANARY\nExecStart=/usr/bin/env python /private/work-router/route_ready.py --token=UNIT_COMMAND_CANARY\n")},
		{RelativePath: "private_user_canary/router.timer", Kind: EntryRegular, Data: []byte("[Timer]\nOnCalendar=CALENDAR_CANARY\n")},
	}}
	process := Process{PID: 404, StartTicks: 90, Executable: "/bin/bash", Arguments: []string{"bash", "/private/roles/wip-dispatch/dispatch_once.sh", "--password=PROCESS_ARGUMENT_CANARY"}, Environment: []string{"TOKEN=PROCESS_ENV_CANARY"}}
	collector.Processes = &fixtureProcesses{input: ProcessInput{Status: StatusAvailable, BootID: "BOOT_ID_CANARY", Before: []Process{process}, After: []Process{process}}}
	managerUnits := []ManagerUnit{
		{Name: "private-user-canary.service", Kind: "service", Commands: []ManagerCommand{{Directive: "ExecStart", Command: "/bin/sh /private/roles/wip-dispatch/wip_dispatch.sh --key=MANAGER_COMMAND_CANARY"}}, Watermark: "MANAGER_WATERMARK_CANARY"},
		{Name: "private-user-canary.timer", Kind: "timer", Schedule: "MANAGER_CALENDAR_CANARY"},
	}
	manager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: managerUnits, After: managerUnits}
	collector.Identity = fixtureIdentity{input: IdentityInput{Status: StatusAvailable, Identity: Identity{HostID: "HOST_ID_CANARY", BootID: "BOOT_ID_CANARY"}}}

	record := collector.Collect()
	if record.Status != StatusUnavailable {
		t.Fatalf("record status = %q, want unavailable until in-flight identities exist", record.Status)
	}
	if record.Controller != testBinding() {
		t.Fatalf("controller binding = %#v, want exact input binding", record.Controller)
	}
	if record.PseudonymousHostID == "HOST_ID_CANARY" || record.PseudonymousBootID == "BOOT_ID_CANARY" {
		t.Fatal("raw host or boot identity was retained")
	}
	if record.DigestSHA256 == "" || len(record.Sources) == 0 {
		t.Fatal("record digest or per-source records missing")
	}
	if record.Capture.Start.IsZero() || record.Capture.Start.After(record.Capture.End) {
		t.Fatalf("invalid global capture window: %#v", record.Capture)
	}

	seenScripts := make(map[string]bool)
	for _, finding := range record.Findings {
		seenScripts[finding.ScriptID] = true
		if finding.PathID == "" {
			t.Fatalf("finding has no keyed path ID: %#v", finding)
		}
		if finding.SourceID == SourceProcessTable && finding.ProcessID == "" {
			t.Fatalf("process finding has no keyed process ID: %#v", finding)
		}
	}
	for _, scriptID := range []string{ScriptWIPLabelReady, ScriptWIPDispatchOnce, ScriptWIPSweepOrphans, ScriptWorkRouter, ScriptHistoricalDispatch} {
		if !seenScripts[scriptID] {
			t.Errorf("known script identifier %q was not found", scriptID)
		}
	}
	for _, finding := range record.Findings {
		if finding.ScriptID == ScriptWorkRouter && finding.Classification == "systemd_unit" && finding.Schedule != "systemd_timer" {
			t.Error("systemd service finding was not joined to its timer classification")
		}
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	for _, canary := range []string{
		"secret_user_canary", "private_user_canary", "HOST_ID_CANARY", "BOOT_ID_CANARY",
		"COMMAND_CANARY", "PRIVATE_ENV_VALUE", "SECRET_ENV_CANARY", "UNIT_CONTENT_CANARY",
		"UNIT_COMMAND_CANARY", "CALENDAR_CANARY", "PROCESS_ARGUMENT_CANARY", "PROCESS_ENV_CANARY",
		"MANAGER_COMMAND_CANARY", "MANAGER_WATERMARK_CANARY", "MANAGER_CALENDAR_CANARY",
		"ANACRON_COMMAND_CANARY", "file-watermark-canary",
	} {
		if bytes.Contains(encoded, []byte(canary)) {
			t.Errorf("serialized record contains secret canary %q", canary)
		}
	}
	if manager.unexpected || !reflect.DeepEqual(manager.queries, []string{QuerySystemManagerUnits, QueryUserManagerUnits}) {
		t.Fatalf("manager queries = %#v, unexpected=%v", manager.queries, manager.unexpected)
	}
	if got := fs.limits["cron.system_file"]; got.MaxBytes == 0 || got.MaxEntries == 0 {
		t.Fatalf("filesystem source was not given explicit limits: %#v", got)
	}
	processReader := collector.Processes.(*fixtureProcesses)
	if processReader.limits.MaxBytes == 0 || processReader.limits.MaxEntries == 0 {
		t.Fatalf("process reader was not given explicit limits: %#v", processReader.limits)
	}
	if limits := manager.limits[QuerySystemManagerUnits]; limits.MaxBytes == 0 || limits.MaxEntries == 0 || limits.MaxCommands == 0 {
		t.Fatalf("manager reader was not given explicit limits: %#v", limits)
	}
}

func TestCollectRetainsMissingDeniedMalformedSymlinkAndRacedStatuses(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs["cron.system_file"] = TreeInput{Status: StatusAbsent}
	fs.inputs["cron.system_directory"] = TreeInput{Status: StatusPermissionDenied}
	fs.inputs["cron.user_allowlisted"] = TreeInput{Status: StatusUnavailable}
	fs.inputs["anacron.configuration"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "anacrontab", Kind: EntryRegular, Data: []byte("not a valid anacron row\n"),
	}}}
	fs.inputs["systemd.system_units"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "outside-link.service", Kind: EntrySymlink,
		Data:      []byte("[Service]\nExecStart=/tmp/label_ready.sh SYMLINK_CONTENT_CANARY\n"),
		Watermark: "symlink-watermark-canary",
	}, {
		RelativePath: "escape", Kind: EntrySymlink,
	}, {
		RelativePath: "escape/linked.service", Kind: EntryRegular,
		Data: []byte("[Service]\nExecStart=/tmp/dispatch_once.sh\n"),
	}}}
	fs.inputs["systemd.generated_units"] = TreeInput{Status: StatusRaced}
	fs.inputs["systemd.runtime_units"] = TreeInput{Status: StatusAvailable, RootSymlink: true}
	collector.Identity = nil // No host identity means a complete process join is unavailable.
	collector.Manager = nil  // The optional manager surface is explicitly unsupported.

	record := collector.Collect()
	if record.Status != StatusUnavailable {
		t.Fatalf("aggregate status = %q, want unavailable", record.Status)
	}
	want := map[string]Status{
		"cron.system_file":        StatusAbsent,
		"cron.system_directory":   StatusPermissionDenied,
		"cron.user_allowlisted":   StatusUnavailable,
		"anacron.configuration":   StatusParseError,
		"systemd.system_units":    StatusParseError,
		"systemd.generated_units": StatusRaced,
		"systemd.runtime_units":   StatusParseError,
		SourceHostIdentity:        StatusUnavailable,
		SourceProcessTable:        StatusUnavailable,
		QuerySystemManagerUnits:   StatusUnsupported,
		SourceInflightIdentities:  StatusUnavailable,
	}
	for id, status := range want {
		if got := sourceByID(t, record, id).Status; got != status {
			t.Errorf("source %q status = %q, want %q", id, got, status)
		}
	}
	if !contains(sourceByID(t, record, "systemd.system_units").IssueCodes, "symlink_rejected") {
		t.Error("symlink was not reported with a sanitized issue code")
	}
	if !contains(sourceByID(t, record, "systemd.runtime_units").IssueCodes, "symlink_rejected") {
		t.Error("root symlink was not reported with a sanitized issue code")
	}
	for _, finding := range record.Findings {
		if finding.PathID == keyedID(collector.HMACKey, "path:systemd.system_units", "escape/linked.service") {
			t.Fatalf("file below a symlink was accepted: %#v", finding)
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	if bytes.Contains(encoded, []byte("SYMLINK_CONTENT_CANARY")) || bytes.Contains(encoded, []byte("symlink-watermark-canary")) {
		t.Error("symlink data or watermark leaked into output")
	}
}

func TestCollectFencesPIDReuse(t *testing.T) {
	collector, _, _ := testCollector()
	collector.Processes = &fixtureProcesses{input: ProcessInput{
		Status: StatusAvailable,
		BootID: "boot-fixture",
		Before: []Process{{PID: 12, StartTicks: 100, Executable: "/bin/sh", Arguments: []string{"/tmp/label_ready.sh"}}},
		After:  []Process{{PID: 12, StartTicks: 101, Executable: "/bin/sh", Arguments: []string{"/tmp/label_ready.sh"}}},
	}}
	record := collector.Collect()
	processSource := sourceByID(t, record, SourceProcessTable)
	if processSource.Status != StatusRaced || !contains(processSource.IssueCodes, "pid_identity_changed") {
		t.Fatalf("process source = %#v, want raced PID reuse", processSource)
	}
	for _, finding := range record.Findings {
		if finding.SourceID == SourceProcessTable {
			t.Fatalf("raced process produced a finding: %#v", finding)
		}
	}
}

func TestProcessShellCommandIndirectionSetsParseError(t *testing.T) {
	for _, shellCommand := range []string{
		"exec \"$RUNNER\"",
		"exec /opt/wip/dispatch_once\".sh\"",
	} {
		t.Run(shellCommand, func(t *testing.T) {
			collector, _, _ := testCollector()
			process := Process{PID: 70, StartTicks: 900, Executable: "/bin/sh", Arguments: []string{"sh", "-c", shellCommand}}
			collector.Processes = &fixtureProcesses{input: ProcessInput{
				Status: StatusAvailable, BootID: "boot-fixture",
				Before: []Process{process}, After: []Process{process},
			}}
			record := collector.Collect()
			source := sourceByID(t, record, SourceProcessTable)
			if source.Status != StatusParseError || !contains(source.IssueCodes, "process_command_text_unsupported") {
				t.Fatalf("process source = %#v, want parse_error for shell -c indirection", source)
			}
			for _, finding := range record.Findings {
				if finding.SourceID == SourceProcessTable {
					t.Fatalf("unresolved shell -c command produced a finding: %#v", finding)
				}
			}
		})
	}
}

func TestOutputOrderingAndControllerJoinAreDeterministic(t *testing.T) {
	first, firstFS, firstManager := testCollector()
	second, secondFS, secondManager := testCollector()
	entries := []TreeEntry{
		{RelativePath: "b.cron", Kind: EntryRegular, Data: []byte("0 * * * * /x/dispatch_once.sh\n")},
		{RelativePath: "a.cron", Kind: EntryRegular, Data: []byte("15 * * * * /x/label_ready.sh\n")},
	}
	firstFS.inputs["cron.system_file"] = TreeInput{Status: StatusAvailable, Entries: append([]TreeEntry(nil), entries...)}
	secondFS.inputs["cron.system_file"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{entries[1], entries[0]}}
	units := []ManagerUnit{
		{Name: "z.service", Kind: "service", Commands: []ManagerCommand{{Directive: "ExecStart", Command: "/x/sweep_orphans.sh"}}},
		{Name: "z.timer", Kind: "timer", Schedule: "hourly"},
	}
	firstManager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: append([]ManagerUnit(nil), units...), After: append([]ManagerUnit(nil), units...)}
	secondManager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: []ManagerUnit{units[1], units[0]}, After: []ManagerUnit{units[1], units[0]}}
	processes := []Process{
		{PID: 20, StartTicks: 200, Executable: "/bin/sh", Arguments: []string{"/x/sweep_orphans.sh"}},
		{PID: 10, StartTicks: 100, Executable: "/usr/bin/python", Arguments: []string{"/x/route_ready.py"}},
	}
	first.Processes = &fixtureProcesses{input: ProcessInput{Status: StatusAvailable, BootID: "boot-fixture", Before: processes, After: processes}}
	second.Processes = &fixtureProcesses{input: ProcessInput{Status: StatusAvailable, BootID: "boot-fixture", Before: []Process{processes[1], processes[0]}, After: []Process{processes[1], processes[0]}}}

	firstRecord, secondRecord := first.Collect(), second.Collect()
	firstJSON, err := json.Marshal(firstRecord)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(secondRecord)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) || firstRecord.DigestSHA256 != secondRecord.DigestSHA256 {
		t.Fatal("input ordering changed serialized inventory or digest")
	}

	joined := JoinStatus(firstRecord, first.Binding)
	if joined.Status != StatusUnavailable || joined.IssueCode != "exact_identity_reader_unavailable" {
		t.Fatalf("matching controller join = %#v, want unavailable pending exact identities", joined)
	}
	mismatch := first.Binding
	mismatch.Generation++
	joined = JoinStatus(firstRecord, mismatch)
	if joined.Status != StatusUnavailable || joined.IssueCode != "controller_snapshot_join_mismatch" {
		t.Fatalf("mismatched controller join = %#v", joined)
	}
}

func TestUnsafePathsAndMalformedCronRemainParseErrors(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs["cron.system_file"] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{
		{RelativePath: "../escape", Kind: EntryRegular, Data: []byte("* * * * * /x/label_ready.sh\n")},
		{RelativePath: "bad.cron", Kind: EntryRegular, Data: []byte("not cron syntax\n")},
	}}
	record := collector.Collect()
	source := sourceByID(t, record, "cron.system_file")
	if source.Status != StatusParseError || !contains(source.IssueCodes, "unsafe_relative_path") || !contains(source.IssueCodes, "cron_entry_malformed") {
		t.Fatalf("unsafe/malformed source = %#v", source)
	}
}

func TestCronScheduleIsCoarsenedAndNumericFieldsDoNotLeak(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "numeric-canary.cron", Kind: EntryRegular,
		Data: []byte("47 22 29 11 6 /x/label_ready.sh\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemCronFile)
	if source.Status != StatusAvailable {
		t.Fatalf("cron source = %q, want available", source.Status)
	}
	found := false
	for _, finding := range record.Findings {
		if finding.SourceID == SourceSystemCronFile && finding.ScriptID == ScriptWIPLabelReady {
			found = true
			if finding.Schedule != "cron_calendar" {
				t.Errorf("schedule = %q, want fixed cron_calendar classification", finding.Schedule)
			}
		}
	}
	if !found {
		t.Fatal("valid numeric cron schedule did not produce its known script finding")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("47 22 29 11 6")) || bytes.Contains(encoded, []byte("numeric-canary")) {
		t.Fatal("raw numeric schedule or path canary leaked into serialized record")
	}
}

func TestCronOutOfRangeAndMalformedSchedulesSetParseError(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "bad.cron", Kind: EntryRegular,
		Data: []byte("60 * * * * /x/label_ready.sh\n1-61 * * * * /x/dispatch_once.sh\n1,,2 * * * * /x/sweep_orphans.sh\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemCronFile)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "cron_entry_malformed") {
		t.Fatalf("cron source = %#v, want parse_error for invalid ranges/grammar", source)
	}
}

func TestCronEnvironmentIndirectionCannotRemainAvailable(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "env.cron", Kind: EntryRegular,
		Data: []byte("RUNNER=/path/dispatch_once.sh\n* * * * * \"$RUNNER\"\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemCronFile)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "cron_environment_indirection_unsupported") {
		t.Fatalf("cron source = %#v, want parse_error for environment indirection", source)
	}
	for _, finding := range record.Findings {
		if finding.SourceID == SourceSystemCronFile && finding.ScriptID == ScriptWIPDispatchOnce {
			t.Fatal("indirect cron command was represented as a resolved finding")
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"RUNNER=/path/dispatch_once.sh", "$RUNNER"} {
		if bytes.Contains(encoded, []byte(canary)) {
			t.Errorf("cron environment canary %q leaked", canary)
		}
	}
}

func TestCronInheritedEnvironmentIndirectionSetsParseError(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "inherited.cron", Kind: EntryRegular,
		Data: []byte("* * * * * \"$RUNNER\"\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemCronFile)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "cron_command_indirection_unsupported") {
		t.Fatalf("cron source = %#v, want parse_error for inherited shell expansion", source)
	}
	if len(record.Findings) != 0 {
		t.Fatalf("unresolved inherited command produced findings: %#v", record.Findings)
	}
}

func TestCronOtherShellIndirectionFormsSetParseError(t *testing.T) {
	for _, command := range []string{
		"`/bin/echo /opt/wip/dispatch_once.sh`",
		"$(/bin/echo /opt/wip/dispatch_once.sh)",
		"/opt/wip/*/dispatch_once.sh",
		"/opt/wip/dispatch_once\".sh\"",
	} {
		t.Run(command, func(t *testing.T) {
			collector, fs, _ := testCollector()
			fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
				RelativePath: "dynamic.cron", Kind: EntryRegular,
				Data: []byte("* * * * * " + command + "\n"),
			}}}
			record := collector.Collect()
			source := sourceByID(t, record, SourceSystemCronFile)
			if source.Status != StatusParseError || !contains(source.IssueCodes, "cron_command_indirection_unsupported") {
				t.Fatalf("cron source = %#v, want parse_error for dynamic command", source)
			}
			for _, finding := range record.Findings {
				if finding.SourceID == SourceSystemCronFile {
					t.Fatalf("dynamic cron command produced a finding: %#v", finding)
				}
			}
		})
	}
}

func TestSystemdEnvironmentAndSpecifierIndirectionSetParseError(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemdSystemUnits] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "indirect.service", Kind: EntryRegular,
		Data: []byte("[Service]\nEnvironment=RUNNER=/path/dispatch_once.sh\nExecStart=/bin/sh \"$RUNNER\" %i\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemdSystemUnits)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "systemd_indirection_unsupported") {
		t.Fatalf("systemd source = %#v, want parse_error for environment/specifier indirection", source)
	}
	for _, finding := range record.Findings {
		if finding.SourceID == SourceSystemdSystemUnits && finding.ScriptID == ScriptWIPDispatchOnce {
			t.Fatal("indirect ExecStart was represented as a resolved finding")
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"RUNNER=/path/dispatch_once.sh", "$RUNNER", "%i"} {
		if bytes.Contains(encoded, []byte(canary)) {
			t.Errorf("systemd indirection canary %q leaked", canary)
		}
	}
}

func TestSystemdEncodedBasenameSetsParseError(t *testing.T) {
	for _, command := range []string{
		`/opt/wip/dispatch_once".sh"`,
		`/opt/wip/dispatch_once\x2esh`,
	} {
		t.Run(command, func(t *testing.T) {
			collector, fs, _ := testCollector()
			fs.inputs[SourceSystemdSystemUnits] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
				RelativePath: "encoded.service", Kind: EntryRegular,
				Data: []byte("[Service]\nExecStart=" + command + "\n"),
			}}}
			record := collector.Collect()
			source := sourceByID(t, record, SourceSystemdSystemUnits)
			if source.Status != StatusParseError || !contains(source.IssueCodes, "systemd_indirection_unsupported") {
				t.Fatalf("systemd source = %#v, want parse_error for encoded basename", source)
			}
			for _, finding := range record.Findings {
				if finding.SourceID == SourceSystemdSystemUnits && finding.ScriptID == ScriptWIPDispatchOnce {
					t.Fatal("encoded systemd command produced a false-clear finding")
				}
			}
		})
	}
}

func TestSystemdClassifiesAllServiceCommandDirectives(t *testing.T) {
	for _, directive := range []string{
		"ExecCondition", "ExecStartPre", "ExecStart", "ExecStartPost", "ExecReload", "ExecStop", "ExecStopPost",
	} {
		t.Run(directive, func(t *testing.T) {
			collector, fs, _ := testCollector()
			fs.inputs[SourceSystemdSystemUnits] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
				RelativePath: "command.service", Kind: EntryRegular,
				Data: []byte("[Service]\n" + directive + "=/opt/wip/dispatch_once.sh\n"),
			}}}
			record := collector.Collect()
			source := sourceByID(t, record, SourceSystemdSystemUnits)
			if source.Status != StatusAvailable {
				t.Fatalf("systemd source = %q, want available for supported %s", source.Status, directive)
			}
			found := false
			for _, finding := range record.Findings {
				if finding.SourceID == SourceSystemdSystemUnits && finding.ScriptID == ScriptWIPDispatchOnce {
					found = true
				}
			}
			if !found {
				t.Fatalf("known script in %s was not classified", directive)
			}
		})
	}
}

func TestSystemdUnknownExecDirectiveFailsClosed(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemdSystemUnits] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "unknown.service", Kind: EntryRegular,
		Data: []byte("[Service]\nExecStartWrapper=/opt/wip/label_ready.sh\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemdSystemUnits)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "systemd_exec_directive_unsupported") {
		t.Fatalf("systemd source = %#v, want parse_error for unsupported command directive", source)
	}
	for _, finding := range record.Findings {
		if finding.SourceID == SourceSystemdSystemUnits {
			t.Fatalf("unsupported command directive produced a finding: %#v", finding)
		}
	}
}

func TestAnacronUsesCoarseScheduleAndRejectsNumericCanaries(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceAnacronConfiguration] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "anacrontab", Kind: EntryRegular,
		Data: []byte("25 37 fixture_job /opt/wip/label_ready.sh\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceAnacronConfiguration)
	if source.Status != StatusAvailable {
		t.Fatalf("anacron source = %q, want available", source.Status)
	}
	found := false
	for _, finding := range record.Findings {
		if finding.SourceID == SourceAnacronConfiguration && finding.ScriptID == ScriptWIPLabelReady {
			found = true
			if finding.Schedule != "anacron_periodic" {
				t.Errorf("schedule = %q, want fixed anacron_periodic classification", finding.Schedule)
			}
		}
	}
	if !found {
		t.Fatal("valid anacron row did not produce its known script finding")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("25 37")) || bytes.Contains(encoded, []byte("period_days:")) {
		t.Fatal("raw anacron period/delay canary leaked into serialized record")
	}
}

func TestAnacronRejectsOutOfRangeAndIndirectRows(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceAnacronConfiguration] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "anacrontab", Kind: EntryRegular,
		Data: []byte("0 0 disabled /opt/wip/label_ready.sh\n" +
			"36501 0 too_long /opt/wip/dispatch_once.sh\n" +
			"1 525601 too_delayed /opt/wip/sweep_orphans.sh\n" +
			"1 0 quote_split /opt/wip/dispatch_once\".sh\"\n" +
			"1 0 indirect \"$RUNNER\"\n"),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceAnacronConfiguration)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "anacron_entry_malformed") || !contains(source.IssueCodes, "anacron_command_indirection_unsupported") {
		t.Fatalf("anacron source = %#v, want malformed and indirect rows to fail closed", source)
	}
	for _, finding := range record.Findings {
		if finding.SourceID == SourceAnacronConfiguration {
			t.Fatalf("invalid or indirect anacron row produced a finding: %#v", finding)
		}
	}
}

func TestManagerBeforeAfterSnapshotChangeIsRaced(t *testing.T) {
	collector, _, manager := testCollector()
	manager.inputs[QuerySystemManagerUnits] = ManagerInput{
		Status: StatusAvailable,
		Before: []ManagerUnit{{Name: "dispatch.service", Kind: "service", Commands: []ManagerCommand{{Directive: "ExecStartPre", Command: "/x/label_ready.sh"}}, Watermark: "before"}},
		After:  []ManagerUnit{{Name: "dispatch.service", Kind: "service", Commands: []ManagerCommand{{Directive: "ExecStartPre", Command: "/x/dispatch_once.sh"}}, Watermark: "after"}},
	}
	record := collector.Collect()
	source := sourceByID(t, record, QuerySystemManagerUnits)
	if source.Status != StatusRaced || !contains(source.IssueCodes, "manager_snapshot_changed") {
		t.Fatalf("manager source = %#v, want raced on changed two-pass snapshot", source)
	}
	for _, finding := range record.Findings {
		if finding.SourceID == QuerySystemManagerUnits {
			t.Fatalf("raced manager snapshot produced a finding: %#v", finding)
		}
	}
}

func TestManagerClassifiesEverySupportedCommandDirective(t *testing.T) {
	for _, directive := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost"} {
		t.Run(directive, func(t *testing.T) {
			collector, _, manager := testCollector()
			units := []ManagerUnit{{
				Name: "known.service", Kind: "service",
				Commands: []ManagerCommand{{Directive: directive, Command: "/x/dispatch_once.sh"}},
			}}
			manager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: units, After: units}
			record := collector.Collect()
			source := sourceByID(t, record, QuerySystemManagerUnits)
			if source.Status != StatusAvailable {
				t.Fatalf("manager source = %q, want available for %s", source.Status, directive)
			}
			for _, finding := range record.Findings {
				if finding.SourceID == QuerySystemManagerUnits && finding.ScriptID == ScriptWIPDispatchOnce {
					return
				}
			}
			t.Fatalf("known script in manager %s was not classified", directive)
		})
	}
}

func TestManagerRejectsUnknownAndIndirectCommandRecords(t *testing.T) {
	tests := []struct {
		name      string
		command   ManagerCommand
		issueCode string
	}{
		{name: "unknown_directive", command: ManagerCommand{Directive: "ExecStartWrapper", Command: "/x/label_ready.sh"}, issueCode: "manager_exec_directive_unsupported"},
		{name: "quote_split", command: ManagerCommand{Directive: "ExecCondition", Command: "/x/dispatch_once\".sh\""}, issueCode: "systemd_indirection_unsupported"},
		{name: "backslash_escape", command: ManagerCommand{Directive: "ExecStart", Command: `/x/dispatch_once\x2esh`}, issueCode: "systemd_indirection_unsupported"},
		{name: "variable", command: ManagerCommand{Directive: "ExecStartPre", Command: "/x/$RUNNER/dispatch_once.sh"}, issueCode: "systemd_indirection_unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector, _, manager := testCollector()
			units := []ManagerUnit{{Name: "indirect.service", Kind: "service", Commands: []ManagerCommand{test.command}}}
			manager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: units, After: units}
			record := collector.Collect()
			source := sourceByID(t, record, QuerySystemManagerUnits)
			if source.Status != StatusParseError || !contains(source.IssueCodes, test.issueCode) {
				t.Fatalf("manager source = %#v, want parse_error for %s", source, test.name)
			}
			for _, finding := range record.Findings {
				if finding.SourceID == QuerySystemManagerUnits {
					t.Fatalf("unsafe manager command produced a finding: %#v", finding)
				}
			}
		})
	}
}

func TestManagerCommandCountAndAggregateByteLimits(t *testing.T) {
	t.Run("command_count", func(t *testing.T) {
		collector, _, manager := testCollector()
		commands := make([]ManagerCommand, maxManagerCommands/2+1)
		for i := range commands {
			commands[i] = ManagerCommand{Directive: "ExecStartPre", Command: "/x/label_ready.sh"}
		}
		units := []ManagerUnit{{Name: "many.service", Kind: "service", Commands: commands}}
		manager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: units, After: units}
		record := collector.Collect()
		source := sourceByID(t, record, QuerySystemManagerUnits)
		if source.Status != StatusParseError || !contains(source.IssueCodes, "manager_metadata_limit_exceeded") {
			t.Fatalf("command-count-limited source = %#v, want parse_error", source)
		}
	})

	t.Run("aggregate_bytes", func(t *testing.T) {
		collector, _, manager := testCollector()
		commandText := strings.Repeat("x", maxManagerCommandBytes)
		commands := make([]ManagerCommand, 33)
		for i := range commands {
			commands[i] = ManagerCommand{Directive: "ExecStartPre", Command: commandText}
		}
		units := []ManagerUnit{{Name: "large.service", Kind: "service", Commands: commands}}
		manager.inputs[QuerySystemManagerUnits] = ManagerInput{Status: StatusAvailable, Before: units, After: units}
		record := collector.Collect()
		source := sourceByID(t, record, QuerySystemManagerUnits)
		if source.Status != StatusParseError || !contains(source.IssueCodes, "manager_metadata_limit_exceeded") {
			t.Fatalf("aggregate-byte-limited source = %#v, want parse_error", source)
		}
	})
}

func TestKeyedIDEnforcesBoundsAndSeparatesDomains(t *testing.T) {
	value := "same-private-value"
	if got := keyedID(bytes.Repeat([]byte{'k'}, 31), "path", value); got != "" {
		t.Fatalf("31-byte HMAC key produced an identifier: %q", got)
	}
	validKey := bytes.Repeat([]byte{'k'}, 32)
	pathID := keyedID(validKey, "path", value)
	hostID := keyedID(validKey, "host", value)
	if pathID == "" || hostID == "" || pathID == hostID {
		t.Fatal("valid HMAC key failed or context domains were not separated")
	}
	if got := keyedID(bytes.Repeat([]byte{'k'}, maxHMACKeyBytes), "path", value); got == "" {
		t.Fatal("maximum accepted HMAC key length was rejected")
	}
	if got := keyedID(bytes.Repeat([]byte{'k'}, maxHMACKeyBytes+1), "path", value); got != "" {
		t.Fatalf("oversized HMAC key produced an identifier: %q", got)
	}
}

func TestFilesystemInputSizeLimitSetsParseError(t *testing.T) {
	collector, fs, _ := testCollector()
	fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: []TreeEntry{{
		RelativePath: "oversized.cron", Kind: EntryRegular,
		Data: bytes.Repeat([]byte{'x'}, maxSingleFileBytes+1),
	}}}
	record := collector.Collect()
	source := sourceByID(t, record, SourceSystemCronFile)
	if source.Status != StatusParseError || !contains(source.IssueCodes, "file_limit_exceeded") {
		t.Fatalf("oversized source = %#v, want bounded parse_error", source)
	}
}

func TestFilesystemAggregateEntryAndByteLimitsSetParseError(t *testing.T) {
	t.Run("entry_count", func(t *testing.T) {
		collector, fs, _ := testCollector()
		entries := make([]TreeEntry, 0, maxTreeEntries+1)
		for i := 0; i <= maxTreeEntries; i++ {
			entries = append(entries, TreeEntry{RelativePath: "entry-" + strconv.Itoa(i), Kind: EntryRegular})
		}
		fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: entries}
		record := collector.Collect()
		source := sourceByID(t, record, SourceSystemCronFile)
		if source.Status != StatusParseError || !contains(source.IssueCodes, "entry_limit_exceeded") {
			t.Fatalf("entry-limited source = %#v, want bounded parse_error", source)
		}
	})

	t.Run("aggregate_bytes", func(t *testing.T) {
		collector, fs, _ := testCollector()
		names := []string{"a.cron", "b.cron", "c.cron", "d.cron", "e.cron", "f.cron", "g.cron", "h.cron", "i.cron"}
		entries := make([]TreeEntry, 0, len(names))
		for _, name := range names {
			entries = append(entries, TreeEntry{
				RelativePath: name, Kind: EntryRegular,
				Data: bytes.Repeat([]byte{'x'}, maxSingleFileBytes),
			})
		}
		fs.inputs[SourceSystemCronFile] = TreeInput{Status: StatusAvailable, Entries: entries}
		record := collector.Collect()
		source := sourceByID(t, record, SourceSystemCronFile)
		if source.Status != StatusParseError || !contains(source.IssueCodes, "byte_limit_exceeded") {
			t.Fatalf("aggregate-byte-limited source = %#v, want bounded parse_error", source)
		}
	})
}

func sourceByID(t *testing.T, record Record, id string) SourceRecord {
	t.Helper()
	for _, source := range record.Sources {
		if source.ID == id {
			return source
		}
	}
	t.Fatalf("source %q missing from record", id)
	return SourceRecord{}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
