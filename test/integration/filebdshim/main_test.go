package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestRunFileStoreReadyExcludesSessionBeads(t *testing.T) {
	cityDir := newShimTestCity(t)
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}
	defer recorder.Close() //nolint:errcheck

	if _, err := store.Create(beads.Bead{Title: "task", Type: "task"}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "session", Type: "session"}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stdout bytes.Buffer
	code, handled, err := runFileStore(cityDir, []string{"ready", "--json"}, &stdout)
	if err != nil {
		t.Fatalf("runFileStore(ready): %v", err)
	}
	if !handled || code != 0 {
		t.Fatalf("runFileStore handled=%v code=%d, want handled=true code=0", handled, code)
	}

	var items []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput=%s", err, stdout.String())
	}
	if len(items) != 1 {
		t.Fatalf("ready returned %d items, want 1\noutput=%s", len(items), stdout.String())
	}
	if got := items[0]["title"]; got != "task" {
		t.Fatalf("ready title = %v, want task", got)
	}
}

func TestProxyRefusesOutsideDisposableBeadsDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	wantBeadsDir := filepath.Join(root, "city", ".beads")
	if err := os.MkdirAll(filepath.Dir(wantBeadsDir), 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	script := "#!/bin/sh\nprintf called > " + strconv.Quote(marker) + "\n"
	if err := os.WriteFile(realBD, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
	t.Setenv("GC_INTEGRATION_DISPOSABLE_BEADS_DIR", wantBeadsDir)
	t.Setenv("GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR", "1")
	t.Setenv("GC_INTEGRATION_DISPOSABLE_CITY_DIR", filepath.Dir(wantBeadsDir))
	t.Setenv("BEADS_DIR", filepath.Join(root, "host", ".beads"))
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"init"}, &stdout, &stderr); code == 0 {
		t.Fatal("run(init) succeeded with a Beads directory outside the disposable fixture")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("real bd marker exists after refusing unsafe target; stat err=%v", err)
	}
}

func TestProxyAllowsExactDisposableBeadsDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	wantBeadsDir := filepath.Join(root, "city", ".beads")
	if err := os.MkdirAll(filepath.Dir(wantBeadsDir), 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	script := "#!/bin/sh\nprintf called > " + strconv.Quote(marker) + "\n"
	if err := os.WriteFile(realBD, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
	t.Setenv("GC_INTEGRATION_DISPOSABLE_BEADS_DIR", wantBeadsDir)
	t.Setenv("GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR", "1")
	t.Setenv("GC_INTEGRATION_DISPOSABLE_CITY_DIR", filepath.Dir(wantBeadsDir))
	t.Setenv("BEADS_DIR", wantBeadsDir)
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(list) code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("real bd marker missing: %v", err)
	}
}

func TestProxyRefusesSymlinkedDisposableBeadsDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	cityDir := filepath.Join(root, "city")
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	wantBeadsDir := filepath.Join(cityDir, ".beads")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("MkdirAll(outside): %v", err)
	}
	if err := os.Symlink(outside, wantBeadsDir); err != nil {
		t.Fatalf("Symlink(Beads directory): %v", err)
	}
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	if err := os.WriteFile(realBD, []byte("#!/bin/sh\nprintf called > "+strconv.Quote(marker)+"\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
	t.Setenv("GC_INTEGRATION_DISPOSABLE_BEADS_DIR", wantBeadsDir)
	t.Setenv("GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR", "1")
	t.Setenv("GC_INTEGRATION_DISPOSABLE_CITY_DIR", cityDir)
	t.Setenv("BEADS_DIR", wantBeadsDir)
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"init"}, &stdout, &stderr); code == 0 {
		t.Fatal("run(init) succeeded with a symlinked Beads directory")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("real bd marker exists after refusing symlinked target; stat err=%v", err)
	}
}

func TestProxyRequiresFixtureLauncherTargetEvenWithoutOptionalSentinel(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	cityDir := filepath.Join(root, "city")
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	if err := os.WriteFile(realBD, []byte("#!/bin/sh\nprintf called > "+strconv.Quote(marker)+"\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
	t.Setenv("GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR", "1")
	t.Setenv("GC_INTEGRATION_DISPOSABLE_CITY_DIR", cityDir)
	t.Setenv("BEADS_DIR", filepath.Join(cityDir, ".beads"))
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"list"}, &stdout, &stderr); code == 0 {
		t.Fatal("run(list) succeeded without the fixture launcher's bound Beads target")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("real bd marker exists after refusing missing target sentinel; stat err=%v", err)
	}
	invalidDatabase, err := json.Marshal(fixtureAuthority{CityDir: cityDir, Database: "host_database"})
	if err != nil {
		t.Fatalf("Marshal(invalid fixture authority): %v", err)
	}
	for _, payload := range []string{
		"not-base64",
		base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
		base64.RawURLEncoding.EncodeToString(invalidDatabase),
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run([]string{fixtureAuthorityFlag + payload, "list"}, &stdout, &stderr); code == 0 {
			t.Errorf("run(list) succeeded with malformed or empty launcher authority %q", payload)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("real bd marker exists after refusing malformed launcher authority; stat err=%v", err)
		}
	}
}

func TestProxyLauncherAuthorityBindsTargetWithoutOptionalSentinels(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	cityDir := filepath.Join(root, "city")
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	beadsDir := filepath.Join(cityDir, ".beads")
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	if err := os.WriteFile(realBD, []byte("#!/bin/sh\nprintf called > "+strconv.Quote(marker)+"\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	authority, err := json.Marshal(fixtureAuthority{CityDir: cityDir, Database: fixtureDatabaseName, Project: "fixture-project"})
	if err != nil {
		t.Fatalf("Marshal(fixture authority): %v", err)
	}
	launcherArg := fixtureAuthorityFlag + base64.RawURLEncoding.EncodeToString(authority)

	t.Run("exact target", func(t *testing.T) {
		t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
		t.Setenv("BEADS_DIR", beadsDir)
		t.Setenv("GC_BEADS", "bd")
		for _, name := range []string{
			"GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR",
			"GC_INTEGRATION_DISPOSABLE_BEADS_DIR",
			"GC_INTEGRATION_DISPOSABLE_CITY_DIR",
			"GC_INTEGRATION_DISPOSABLE_DATABASE",
			"GC_INTEGRATION_DISPOSABLE_PROJECT_ID",
		} {
			t.Setenv(name, "")
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{launcherArg, "list"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run(list) code=%d stderr=%s", code, stderr.String())
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("real bd marker missing for the exact launcher-bound target: %v", err)
		}
	})

	t.Run("mismatched target", func(t *testing.T) {
		if err := os.Remove(marker); err != nil {
			t.Fatalf("Remove(marker): %v", err)
		}
		t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
		t.Setenv("BEADS_DIR", filepath.Join(root, "host", ".beads"))
		t.Setenv("GC_BEADS", "bd")
		for _, name := range []string{
			"GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR",
			"GC_INTEGRATION_DISPOSABLE_BEADS_DIR",
			"GC_INTEGRATION_DISPOSABLE_CITY_DIR",
			"GC_INTEGRATION_DISPOSABLE_DATABASE",
			"GC_INTEGRATION_DISPOSABLE_PROJECT_ID",
		} {
			t.Setenv(name, "")
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{launcherArg, "list"}, &stdout, &stderr); code == 0 {
			t.Fatal("run(list) succeeded with a mismatched Beads path and no optional fixture sentinels")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("real bd marker exists after refusing a mismatched launcher-bound target; stat err=%v", err)
		}
	})
}

func TestProxyRejectsAlternateFixtureScopeArgumentsBeforeExec(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	cityDir := filepath.Join(root, "city")
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	beadsDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	outside := filepath.Join(root, "other-city")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("MkdirAll(other city): %v", err)
	}
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	if err := os.WriteFile(realBD, []byte("#!/bin/sh\nprintf called >> "+strconv.Quote(marker)+"\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
	t.Setenv("GC_INTEGRATION_DISPOSABLE_BEADS_DIR", beadsDir)
	t.Setenv("GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR", "1")
	t.Setenv("GC_INTEGRATION_DISPOSABLE_CITY_DIR", cityDir)
	t.Setenv("GC_INTEGRATION_DISPOSABLE_DATABASE", fixtureDatabaseName)
	t.Setenv("GC_INTEGRATION_DISPOSABLE_PROJECT_ID", "fixture-project")
	t.Setenv("BEADS_DIR", beadsDir)
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	allowed := []string{"list", "-C", cityDir, "--directory=" + cityDir, "--database", fixtureDatabaseName, "--project-id=fixture-project"}
	if code := run(allowed, &stdout, &stderr); code != 0 {
		t.Fatalf("run(exact fixture overrides) code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("real bd marker missing after exact fixture overrides: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("Remove(marker): %v", err)
	}

	for _, args := range [][]string{
		{"list", "-C", outside},
		{"list", "--directory=" + outside},
		{"list", "-C" + outside},
		{"list", "-Cother-city"},
		{"list", "--dir", outside},
		{"list", "--beads-dir", filepath.Join(outside, ".beads")},
		{"list", "--db", "other_db"},
		{"list", "--database=other_db"},
		{"list", "--db", filepath.Join(outside, "beads.db")},
		{"list", "--server-host", "host.invalid"},
		{"list", "--server-port", "3307"},
		{"list", "--server-socket", filepath.Join(outside, "beads.sock")},
		{"init", "--server-config", filepath.Join(outside, "server.yaml"), cityDir},
		{"init", "--host", "host.invalid", cityDir},
		{"init", "--port", "3307", cityDir},
		{"init", "--socket", filepath.Join(outside, "beads.sock"), cityDir},
		{"init", "--proxied-server-external-port", "3307", cityDir},
		{"init", "--proxied-server-root-path", filepath.Join(outside, "dolt"), cityDir},
		{"init", "--proxied-server-config-path", filepath.Join(outside, "config.yaml"), cityDir},
		{"init", "--proxied-server-log-path", filepath.Join(outside, "server.log"), cityDir},
		{"init", "--proxied-server-external-host", "host.invalid", cityDir},
		{"list", "--global"},
		{"list", "--global=true"},
		{"list", "--server=false"},
		{"list", "--shared-server=false"},
		{"init", "--server", cityDir},
		{"list", "--project", "other-project"},
		{"init", outside, "other-prefix"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code == 0 {
			t.Errorf("run(%v) succeeded with an alternate fixture scope", args)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("real bd marker exists after refusing %v; stat err=%v", args, err)
		}
	}
}

func TestProxyRejectsAmbientBeadsEndpointSelectorsBeforeExec(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	cityDir := filepath.Join(root, "city")
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	beadsDir := filepath.Join(cityDir, ".beads")
	marker := filepath.Join(root, "real-bd-called")
	realBD := filepath.Join(root, "bd")
	if err := os.WriteFile(realBD, []byte("#!/bin/sh\nprintf called > "+strconv.Quote(marker)+"\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(real bd): %v", err)
	}
	authority, err := json.Marshal(fixtureAuthority{CityDir: cityDir, Database: fixtureDatabaseName, Project: "fixture-project"})
	if err != nil {
		t.Fatalf("Marshal(fixture authority): %v", err)
	}
	launcherArg := fixtureAuthorityFlag + base64.RawURLEncoding.EncodeToString(authority)
	selectors := []string{
		"BEADS_PROXIED_SERVER_ROOT_PATH", "BEADS_PROXIED_SERVER_CONFIG", "BEADS_PROXIED_SERVER_LOG",
		"BEADS_PROXIED_SERVER_PORT", "BEADS_PROXIED_SERVER_EXTERNAL_HOST", "BEADS_PROXIED_SERVER_EXTERNAL_PORT", "BEADS_PROXIED_SERVER_EXTERNAL_SOCKET_PATH",
		"BEADS_DOLT_DATA_DIR", "BEADS_DOLT_SHARED_SERVER", "BEADS_SHARED_SERVER_DIR",
		"BEADS_DOLT_DATABASE", "BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_SERVER_MODE", "BEADS_DOLT_SERVER_SOCKET",
		"BEADS_DOLT_SERVER_TLS", "BEADS_DOLT_REMOTESAPI_PORT", "BEADS_DOLT_CREDENTIAL_COMMAND",
		"BEADS_DB", "BD_DB", "BEADS_CENTRAL_CONFIG",
		"GC_BEADS_PROXY_EXTERNAL_HOST", "GC_BEADS_PROXY_EXTERNAL_PORT", "GC_BEADS_PROXY_EXTERNAL_SOCKET",
	}
	for name, value := range map[string]string{
		"BEADS_PROXIED_SERVER_ROOT_PATH":            filepath.Join(root, "other", "dolt"),
		"BEADS_PROXIED_SERVER_CONFIG":               filepath.Join(root, "other", "config.yaml"),
		"BEADS_PROXIED_SERVER_LOG":                  filepath.Join(root, "other", "server.log"),
		"BEADS_DOLT_DATA_DIR":                       filepath.Join(root, "other", "dolt"),
		"BEADS_DOLT_SHARED_SERVER":                  "1",
		"BEADS_SHARED_SERVER_DIR":                   filepath.Join(root, "shared-server"),
		"BEADS_DOLT_DATABASE":                       "host_database",
		"BEADS_DOLT_SERVER_DATABASE":                "host_database",
		"BEADS_DOLT_SERVER_MODE":                    "1",
		"BEADS_DOLT_SERVER_SOCKET":                  filepath.Join(root, "host.sock"),
		"BEADS_DOLT_SERVER_TLS":                     "1",
		"BEADS_DOLT_REMOTESAPI_PORT":                "3308",
		"BEADS_DOLT_CREDENTIAL_COMMAND":             "echo unexpected",
		"BEADS_PROXIED_SERVER_PORT":                 "3307",
		"BEADS_PROXIED_SERVER_EXTERNAL_HOST":        "host.invalid",
		"BEADS_PROXIED_SERVER_EXTERNAL_PORT":        "3307",
		"BEADS_PROXIED_SERVER_EXTERNAL_SOCKET_PATH": filepath.Join(root, "host.sock"),
		"BEADS_DB":                       filepath.Join(root, "host.db"),
		"BD_DB":                          filepath.Join(root, "host.db"),
		"BEADS_CENTRAL_CONFIG":           filepath.Join(root, "host-config.yaml"),
		"GC_BEADS_PROXY_EXTERNAL_HOST":   "host.invalid",
		"GC_BEADS_PROXY_EXTERNAL_PORT":   "3307",
		"GC_BEADS_PROXY_EXTERNAL_SOCKET": filepath.Join(root, "host.sock"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				t.Fatalf("Remove(marker): %v", err)
			}
			t.Setenv("GC_INTEGRATION_REAL_BD", realBD)
			t.Setenv("BEADS_DIR", beadsDir)
			t.Setenv("GC_BEADS", "bd")
			for _, key := range []string{
				"GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR",
				"GC_INTEGRATION_DISPOSABLE_BEADS_DIR",
				"GC_INTEGRATION_DISPOSABLE_CITY_DIR",
				"GC_INTEGRATION_DISPOSABLE_DATABASE",
				"GC_INTEGRATION_DISPOSABLE_PROJECT_ID",
			} {
				t.Setenv(key, "")
			}
			for _, key := range selectors {
				t.Setenv(key, "")
			}
			t.Setenv(name, value)
			var stdout, stderr bytes.Buffer
			if code := run([]string{launcherArg, "list"}, &stdout, &stderr); code == 0 {
				t.Fatalf("run(list) succeeded with %s override", name)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("real bd marker exists after refusing %s override; stat err=%v", name, err)
			}
		})
	}
}

func TestRunFileStoreReadyRespectsAssigneeFilter(t *testing.T) {
	cityDir := newShimTestCity(t)
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}
	defer recorder.Close() //nolint:errcheck

	task, err := store.Create(beads.Bead{Title: "claimed-task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	if err := store.Update(task.ID, beads.UpdateOpts{Assignee: stringPtr("worker")}); err != nil {
		t.Fatalf("Update(task assignee): %v", err)
	}
	session, err := store.Create(beads.Bead{Title: "worker-session", Type: "session"})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	if err := store.Update(session.ID, beads.UpdateOpts{Assignee: stringPtr("worker")}); err != nil {
		t.Fatalf("Update(session assignee): %v", err)
	}

	var stdout bytes.Buffer
	code, handled, err := runFileStore(cityDir, []string{"ready", "--assignee=worker", "--json"}, &stdout)
	if err != nil {
		t.Fatalf("runFileStore(ready --assignee): %v", err)
	}
	if !handled || code != 0 {
		t.Fatalf("runFileStore handled=%v code=%d, want handled=true code=0", handled, code)
	}

	var items []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput=%s", err, stdout.String())
	}
	if len(items) != 1 {
		t.Fatalf("ready returned %d items, want 1\noutput=%s", len(items), stdout.String())
	}
	if got := items[0]["title"]; got != "claimed-task" {
		t.Fatalf("ready title = %v, want claimed-task", got)
	}
}

func newShimTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatalf("openFileStore(init): %v", err)
	}
	defer recorder.Close() //nolint:errcheck
	if _, err := store.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List(init): %v", err)
	}
	return cityDir
}

func stringPtr(s string) *string {
	return &s
}
