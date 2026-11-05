package config

import "testing"

func TestRuntimeFingerprintIgnoresUnrelatedBuiltinSettings(t *testing.T) {
	cfg := &City{Workspace: Workspace{Name: "city"}, Session: SessionConfig{Provider: "herdr"}}
	before, err := RuntimeBindingForName(cfg, "herdr")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Session.K8s.Namespace = "other"
	cfg.Session.ACP.OutputBufferLines = 222
	after, err := RuntimeBindingForName(cfg, "herdr")
	if err != nil || before != after {
		t.Fatal("unrelated K8s/ACP change retargeted Herdr")
	}
	cfg.Session.SetupTimeout = "45s"
	after, err = RuntimeBindingForName(cfg, "herdr")
	if err != nil || before == after {
		t.Fatal("effective Herdr setting not fingerprinted")
	}
}

func runtimeSelectionFixture() *City {
	return &City{Session: SessionConfig{Provider: "herdr"}, Runtimes: map[string]DiscoveredRuntime{
		"herdr":     {Name: "herdr", Command: "/pack/herdr"},
		"capsule":   {Name: "capsule", Command: "/pack/capsule"},
		"alternate": {Name: "alternate", Command: "/pack/alternate"},
	}, Agents: []Agent{{Name: "helper", Runtime: "capsule"}}, NamedSessions: []NamedSession{{Name: "named", Template: "helper", Runtime: "alternate"}}}
}

func TestRuntimeSelectionKeepsThreeAxesSeparate(t *testing.T) {
	cfg := runtimeSelectionFixture()
	template, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "")
	if err != nil || template.Name != "capsule" {
		t.Fatalf("template runtime=%#v,%v", template, err)
	}
	named, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "named", "")
	if err != nil || named.Name != "alternate" {
		t.Fatalf("named runtime=%#v,%v", named, err)
	}
	cfg.Agents[0].Runtime = ""
	base, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "")
	if err != nil || base.Name != "herdr" {
		t.Fatalf("base runtime=%#v,%v", base, err)
	}
	acp, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "acp")
	if err != nil || acp.Name != "acp" {
		t.Fatalf("ACP runtime=%#v,%v", acp, err)
	}
	cfg.Agents[0].Runtime = "capsule"
	if _, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "acp"); err == nil {
		t.Fatal("explicit exec runtime ignored in favor of ACP")
	}
	cfg.Agents[0].Runtime = "misspelled"
	if _, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", ""); err == nil {
		t.Fatal("unknown runtime silently accepted")
	}
}

func TestRuntimeFingerprintTracksSelectedDeclarationOnly(t *testing.T) {
	cfg := runtimeSelectionFixture()
	before, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "")
	if err != nil {
		t.Fatal(err)
	}
	other := cfg.Runtimes["alternate"]
	other.Command = "/unrelated/update"
	cfg.Runtimes["alternate"] = other
	cfg.Agents[0].Provider = "another-coding-provider"
	after, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "")
	if err != nil || before != after {
		t.Fatalf("unrelated declaration/provider changed runtime: %#v %#v %v", before, after, err)
	}
	selected := cfg.Runtimes["capsule"]
	selected.Command = "/selected/update"
	cfg.Runtimes["capsule"] = selected
	after, err = ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "", "")
	if err != nil || before.Fingerprint == after.Fingerprint {
		t.Fatal("selected declaration drift not fingerprinted")
	}
}

func TestRuntimeSelectionOverridesAndClearing(t *testing.T) {
	cfg := runtimeSelectionFixture()
	if err := ApplyPatches(cfg, Patches{Agents: []AgentPatch{{Name: "helper", Runtime: ptrStr("alternate")}}, NamedSessions: []NamedSessionPatch{{Name: "named", Runtime: ptrStr("capsule")}}}); err != nil {
		t.Fatal(err)
	}
	selected, err := ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "named", "")
	if err != nil || selected.Name != "capsule" {
		t.Fatalf("named override=%#v %v", selected, err)
	}
	if err := ApplyPatches(cfg, Patches{NamedSessions: []NamedSessionPatch{{Name: "named", Runtime: ptrStr("")}}}); err != nil {
		t.Fatal(err)
	}
	selected, err = ResolveSessionRuntimeBinding(cfg, "herdr", "helper", "named", "")
	if err != nil || selected.Name != "alternate" {
		t.Fatalf("cleared override did not inherit template: %#v %v", selected, err)
	}
}
