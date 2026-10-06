// Package main provides a tiny bd-compatible shim backed by file beads for
// integration tests that need deterministic local bead operations.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

func run(args []string, stdout, stderr io.Writer) int {
	authority, args, err := consumeFixtureAuthority(args)
	if err != nil {
		fmt.Fprintln(stderr, "bd shim: refusing invalid fixture launcher authority") //nolint:errcheck
		return 1
	}
	realBD := strings.TrimSpace(os.Getenv("GC_INTEGRATION_REAL_BD"))
	if err := requireDisposableBeadsDir(args, authority); err != nil {
		fmt.Fprintln(stderr, "bd shim: refusing real bd outside the disposable fixture") //nolint:errcheck
		return 1
	}
	if len(args) == 0 {
		return proxy(realBD, args, stdout, stderr)
	}

	cityDir, ok := detectFileStoreCity()
	if !ok {
		return proxy(realBD, args, stdout, stderr)
	}

	code, handled, err := runFileStore(cityDir, args, stdout)
	if !handled {
		return proxy(realBD, args, stdout, stderr)
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err) //nolint:errcheck
		return 1
	}
	return code
}

func proxy(realBD string, args []string, stdout, stderr io.Writer) int {
	if realBD == "" {
		fmt.Fprintln(stderr, "bd shim: GC_INTEGRATION_REAL_BD not set") //nolint:errcheck
		return 1
	}
	cmd := exec.Command(realBD, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "bd shim: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

const (
	fixtureAuthorityFlag = "--gc-integration-fixture-authority="
	fixtureDatabaseName  = "hq"
)

type fixtureAuthority struct {
	CityDir  string `json:"city_dir"`
	Database string `json:"database"`
	Project  string `json:"project_id"`
	bound    bool
}

func consumeFixtureAuthority(args []string) (fixtureAuthority, []string, error) {
	var authority fixtureAuthority
	for i, arg := range args {
		if !strings.HasPrefix(arg, fixtureAuthorityFlag) {
			continue
		}
		if i != 0 || authority.bound {
			return fixtureAuthority{}, nil, errors.New("fixture authority must be the first argument")
		}
		encoded := strings.TrimPrefix(arg, fixtureAuthorityFlag)
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 {
			return fixtureAuthority{}, nil, errors.New("fixture authority is malformed")
		}
		if err := json.Unmarshal(data, &authority); err != nil {
			return fixtureAuthority{}, nil, errors.New("fixture authority is malformed")
		}
		if strings.TrimSpace(authority.CityDir) == "" || !filepath.IsAbs(authority.CityDir) {
			return fixtureAuthority{}, nil, errors.New("fixture authority city is missing or relative")
		}
		if authority.Database != fixtureDatabaseName {
			return fixtureAuthority{}, nil, errors.New("fixture authority database does not match the disposable city")
		}
		authority.bound = true
		args = args[1:]
		break
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, fixtureAuthorityFlag) {
			return fixtureAuthority{}, nil, errors.New("fixture authority may not appear as a bd argument")
		}
	}
	return authority, args, nil
}

func requireDisposableBeadsDir(args []string, authority fixtureAuthority) error {
	expected := strings.TrimSpace(os.Getenv("GC_INTEGRATION_DISPOSABLE_BEADS_DIR"))
	required := strings.TrimSpace(os.Getenv("GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR")) == "1"
	cityDir := strings.TrimSpace(os.Getenv("GC_INTEGRATION_DISPOSABLE_CITY_DIR"))
	database := strings.TrimSpace(os.Getenv("GC_INTEGRATION_DISPOSABLE_DATABASE"))
	projectID := strings.TrimSpace(os.Getenv("GC_INTEGRATION_DISPOSABLE_PROJECT_ID"))
	if authority.bound {
		cityDir = filepath.Clean(authority.CityDir)
		boundBeadsDir := filepath.Join(cityDir, ".beads")
		if expected != "" && filepath.Clean(expected) != boundBeadsDir {
			return errors.New("fixture target conflicts with launcher authority")
		}
		if database != "" && authority.Database != "" && database != authority.Database {
			return errors.New("fixture database conflicts with launcher authority")
		}
		if projectID != "" && authority.Project != "" && projectID != authority.Project {
			return errors.New("fixture project conflicts with launcher authority")
		}
		expected = boundBeadsDir
		database = authority.Database
		projectID = authority.Project
		required = true
		if err := validateFixtureBackendSelectors(expected, database, projectID); err != nil {
			return err
		}
	}
	if expected == "" {
		if required {
			return errors.New("required fixture target is missing")
		}
		return nil
	}
	if cityDir == "" {
		cityDir = filepath.Dir(expected)
	}
	actual := strings.TrimSpace(os.Getenv("BEADS_DIR"))
	if !filepath.IsAbs(expected) || !filepath.IsAbs(cityDir) || !filepath.IsAbs(actual) {
		return errors.New("fixture paths must be absolute")
	}
	expected, err := filepath.Abs(expected)
	if err != nil {
		return err
	}
	cityDir, err = filepath.Abs(cityDir)
	if err != nil {
		return err
	}
	actual, err = filepath.Abs(actual)
	if err != nil {
		return err
	}
	expected = filepath.Clean(expected)
	cityDir = filepath.Clean(cityDir)
	if expected != filepath.Join(cityDir, ".beads") || !fixturePathMatches(cityDir, cityDir, false) {
		return errors.New("fixture Beads directory is outside its city")
	}
	if !fixturePathMatches(actual, expected, true) {
		return errors.New("beads directory does not match fixture")
	}
	if err := validateBdScopeArgs(args, cityDir, expected, database, projectID); err != nil {
		return err
	}
	return nil
}

func validateFixtureBackendSelectors(beadsDir, database, projectID string) error {
	root := filepath.Join(beadsDir, "dolt")
	for name, expected := range map[string]string{
		"BEADS_PROXIED_SERVER_ROOT_PATH": root,
		"BEADS_DOLT_DATA_DIR":            root,
		"GC_DOLT_DATA_DIR":               root,
		"BEADS_PROXIED_SERVER_CONFIG":    filepath.Join(root, "config.yaml"),
		"BEADS_PROXIED_SERVER_LOG":       filepath.Join(root, "server.log"),
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && (!filepath.IsAbs(value) || filepath.Clean(value) != expected) {
			return fmt.Errorf("%s is outside the fixture's proxied root", name)
		}
	}
	for _, name := range []string{
		"BEADS_SHARED_SERVER_DIR", "BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_SERVER_SOCKET",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_USER", "BEADS_DOLT_SERVER_PASSWORD",
		"BEADS_DOLT_SERVER_TLS", "BEADS_DOLT_REMOTESAPI_PORT", "BEADS_DOLT_CREDENTIAL_COMMAND",
		"BEADS_DOLT_HOST", "BEADS_DOLT_PORT", "BEADS_DOLT_SOCKET", "BEADS_DOLT_USER", "BEADS_DOLT_PASSWORD",
		"BEADS_PROXIED_SERVER_PORT", "BEADS_PROXIED_SERVER_EXTERNAL_HOST", "BEADS_PROXIED_SERVER_EXTERNAL_PORT", "BEADS_PROXIED_SERVER_EXTERNAL_SOCKET_PATH",
		"BD_DB", "BEADS_DB", "BEADS_CENTRAL_CONFIG",
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD",
		"GC_DOLT_DATABASE", "GC_DOLT_CONFIG_FILE", "GC_DOLT_LOG_FILE", "GC_DOLT_PID_FILE",
		"GC_DOLT_LOCK_FILE", "GC_DOLT_STATE_FILE",
		"GC_BEADS_PROXY_EXTERNAL_HOST", "GC_BEADS_PROXY_EXTERNAL_PORT", "GC_BEADS_PROXY_EXTERNAL_SOCKET",
	} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return fmt.Errorf("%s is not supported by the fixture's local proxied backend", name)
		}
	}
	if value := strings.TrimSpace(os.Getenv("BEADS_DOLT_SHARED_SERVER")); value != "" && value != "0" && !strings.EqualFold(value, "false") {
		return errors.New("shared Beads server mode is not supported by the fixture")
	}
	if value := strings.TrimSpace(os.Getenv("BEADS_DOLT_SERVER_MODE")); value != "" && value != "0" && !strings.EqualFold(value, "false") {
		return errors.New("direct Beads server mode is not supported by the fixture")
	}
	if value := strings.TrimSpace(os.Getenv("BEADS_DOLT_PROXIED_SERVER")); value != "" && value != "1" {
		return errors.New("proxied Beads server selector conflicts with the fixture")
	}
	if value := strings.TrimSpace(os.Getenv("GC_BEADS_TRANSPORT")); value != "" && value != "proxied" {
		return errors.New("beads transport selector conflicts with the fixture")
	}
	if value := strings.TrimSpace(os.Getenv("GC_BEADS_TARGET")); value != "" && value != "local" {
		return errors.New("beads target selector conflicts with the fixture")
	}
	for _, name := range []string{"GC_BEADS_BACKEND", "BEADS_BACKEND"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && value != "dolt" {
			return fmt.Errorf("%s does not select the fixture's Dolt backend", name)
		}
	}
	if value := strings.TrimSpace(os.Getenv("GC_BEADS")); value != "" && value != "bd" {
		return errors.New("beads command selector conflicts with the fixture")
	}
	if value := strings.TrimSpace(os.Getenv("BEADS_DOLT_DATABASE")); value != "" && (database == "" || value != database) {
		return errors.New("beads database selector conflicts with the fixture")
	}
	if value := strings.TrimSpace(os.Getenv("GC_INTEGRATION_DISPOSABLE_PROJECT_ID")); value != "" && (projectID == "" || value != projectID) {
		return errors.New("fixture project selector conflicts with launcher authority")
	}
	return nil
}

func fixturePathMatches(path, expected string, allowMissing bool) bool {
	if !filepath.IsAbs(path) || !filepath.IsAbs(expected) {
		return false
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	expected, err = filepath.Abs(expected)
	if err != nil {
		return false
	}
	path = filepath.Clean(path)
	expected = filepath.Clean(expected)
	if path != expected {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved) == path
	}
	if !allowMissing || !errors.Is(err, os.ErrNotExist) {
		return false
	}
	ancestor := path
	for {
		resolvedAncestor, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			return filepath.Clean(resolvedAncestor) == ancestor
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false
		}
		ancestor = parent
	}
}

func validateBdScopeArgs(args []string, cityDir, beadsDir, database, projectID string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--proxied-server-external-") || strings.HasPrefix(arg, "--server-") || strings.HasPrefix(arg, "--external-") {
			return errors.New("server endpoint override is not supported by the fixture's local proxied backend")
		}
		name, value, inline := splitFixtureTargetFlag(arg)
		switch name {
		case "-C", "--directory", "--dir":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("directory override is incomplete")
				}
				i++
				value = args[i]
			}
			if !fixturePathMatches(value, cityDir, false) {
				return errors.New("directory override is outside the fixture city")
			}
		case "--beads-dir":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("beads directory override is incomplete")
				}
				i++
				value = args[i]
			}
			if !fixturePathMatches(value, beadsDir, true) {
				return errors.New("beads directory override is outside the fixture city")
			}
		case "--db":
			return errors.New("--db path overrides are not supported by the fixture")
		case "--database":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("database override is incomplete")
				}
				i++
				value = args[i]
			}
			if database == "" || value != database {
				return errors.New("database override does not match the fixture")
			}
		case "--project", "--project-id":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("project override is incomplete")
				}
				i++
				value = args[i]
			}
			if projectID == "" || value != projectID {
				return errors.New("project override does not match the fixture")
			}
		case "--server", "--shared-server", "--external", "--global", "--server-tls":
			return errors.New("direct, shared, and external server modes are not supported by the fixture")
		case "--server-host", "--server-port", "--server-socket", "--server-user", "--proxied-server-port", "--host", "--port", "--socket", "--user":
			return errors.New("endpoint override is not supported by the fixture's local proxied backend")
		case "--proxied-server-idle-timeout":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("proxied idle-timeout override is incomplete")
				}
				i++
				value = args[i]
			}
			if value != "0" {
				return errors.New("fixture proxied-server idle timeout must stay disabled")
			}
		case "--proxied-server-root-path":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("proxied root override is incomplete")
				}
				i++
				value = args[i]
			}
			if !fixturePathMatches(value, filepath.Join(beadsDir, "dolt"), true) {
				return errors.New("proxied root override is outside the fixture city")
			}
		case "--proxied-server-config-path":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("proxied config override is incomplete")
				}
				i++
				value = args[i]
			}
			if !fixturePathMatches(value, filepath.Join(beadsDir, "dolt", "config.yaml"), true) {
				return errors.New("proxied config override is outside the fixture city")
			}
		case "--proxied-server-log-path":
			if !inline {
				if i+1 >= len(args) {
					return errors.New("proxied log override is incomplete")
				}
				i++
				value = args[i]
			}
			if !fixturePathMatches(value, filepath.Join(beadsDir, "dolt", "server.log"), true) {
				return errors.New("proxied log override is outside the fixture city")
			}
		}
	}

	if len(args) > 0 && args[0] == "init" {
		path, found, err := bdInitDirectoryArgument(args[1:])
		if err != nil {
			return err
		}
		if found {
			if !fixturePathMatches(path, cityDir, false) {
				return errors.New("init directory is outside the fixture city")
			}
		} else {
			cwd, err := os.Getwd()
			if err != nil || !fixturePathMatches(cwd, cityDir, false) {
				return errors.New("init working directory is outside the fixture city")
			}
		}
	}
	return nil
}

func splitFixtureTargetFlag(arg string) (name, value string, inline bool) {
	// pflag accepts attached values for the persistent -C shorthand (for
	// example `-C/other-city`) as well as the conventional `-C=/other-city`.
	// Normalize both before validating the effective city directory.
	if strings.HasPrefix(arg, "-C") && len(arg) > 2 && arg[2] != '-' {
		return "-C", strings.TrimPrefix(arg[2:], "="), true
	}
	if equals := strings.IndexByte(arg, '='); equals > 0 {
		flagName := arg[:equals]
		switch flagName {
		case "--server", "--shared-server", "--external", "--global", "--server-tls":
			// These are selectors, so rejecting `=false` is safer than treating
			// one spelling as harmless while another selects a shared backend.
			return flagName, arg[equals+1:], true
		}
	}
	for _, name := range []string{
		"--directory", "--beads-dir", "--database", "--project-id", "--project", "--db", "--dir", "-C",
		"--server-host", "--server-port", "--server-socket", "--proxied-server-idle-timeout",
		"--proxied-server-port",
		"--proxied-server-root-path", "--proxied-server-config-path", "--proxied-server-log-path",
		"--proxied-server-external-host", "--proxied-server-external-port", "--proxied-server-external-socket-path",
	} {
		if strings.HasPrefix(arg, name+"=") {
			return name, strings.TrimPrefix(arg, name+"="), true
		}
	}
	return arg, "", false
}

func bdInitDirectoryArgument(args []string) (string, bool, error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1], true, nil
			}
			return "", false, nil
		}
		if !strings.HasPrefix(arg, "-") {
			return arg, true, nil
		}
		if fixtureFlagTakesValue(arg) {
			if i+1 >= len(args) {
				return "", false, errors.New("init option is incomplete")
			}
			i++
		}
	}
	return "", false, nil
}

func fixtureFlagTakesValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "-C", "--directory", "--dir", "--beads-dir", "--db", "--database", "--project", "--project-id",
		"-p", "--prefix", "--server-host", "--server-port", "--server-socket", "--proxied-server-idle-timeout", "--proxied-server-port", "--host", "--port", "--user",
		"--proxied-server-root-path", "--proxied-server-config-path", "--proxied-server-log-path",
		"--proxied-server-external-host", "--proxied-server-external-port", "--proxied-server-external-socket-path",
		"--actor", "--format", "--limit", "--title", "--assignee", "--type", "--issue-type", "--parent",
		"--description", "--label":
		return true
	default:
		return false
	}
}

func detectFileStoreCity() (string, bool) {
	if strings.TrimSpace(os.Getenv("GC_BEADS")) != "file" {
		return "", false
	}
	candidates := []string{
		strings.TrimSpace(os.Getenv("GC_CITY")),
		strings.TrimSpace(os.Getenv("GC_CITY_PATH")),
	}
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		if hasFileStore(cand) {
			return cand, true
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for dir := cwd; dir != "" && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if hasFileStore(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", false
}

func hasFileStore(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".gc", "beads.json"))
	return err == nil
}

func runFileStore(cityDir string, args []string, stdout io.Writer) (int, bool, error) {
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		return 0, false, err
	}
	defer recorder.Close() //nolint:errcheck

	switch args[0] {
	case "create":
		title, jsonOut, err := parseCreateArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		created, err := store.Create(beads.Bead{Title: title})
		if err != nil {
			return 0, true, err
		}
		record(recorder, events.BeadCreated, actor(), created.ID, created.Title)
		return 0, true, writeBead(stdout, created, jsonOut, true)
	case "show":
		id, jsonOut, err := parseShowArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		b, err := store.Get(id)
		if err != nil {
			return 0, true, err
		}
		return 0, true, writeBead(stdout, b, jsonOut, false)
	case "list":
		q, jsonOut, err := parseListArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		items, err := store.List(q)
		if err != nil {
			return 0, true, err
		}
		return 0, true, writeList(stdout, items, jsonOut)
	case "ready":
		q, jsonOut, err := parseReadyArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		items, err := store.Ready()
		if err != nil {
			return 0, true, err
		}
		items = beads.ApplyListQuery(items, q)
		return 0, true, writeList(stdout, items, jsonOut)
	case "close":
		id, jsonOut, err := parseCloseArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		if err := store.Close(id); err != nil {
			return 0, true, err
		}
		record(recorder, events.BeadClosed, actor(), id, "")
		if jsonOut {
			_, _ = fmt.Fprintln(stdout, `{"ok":true}`)
		}
		return 0, true, nil
	case "update":
		id, opts, jsonOut, err := parseUpdateArgs(args[1:])
		if err != nil {
			return 0, true, err
		}
		if err := store.Update(id, opts); err != nil {
			return 0, true, err
		}
		record(recorder, events.BeadUpdated, actor(), id, "")
		if jsonOut {
			b, err := store.Get(id)
			if err != nil {
				return 0, true, err
			}
			return 0, true, writeBead(stdout, b, true, false)
		}
		return 0, true, nil
	default:
		return 0, false, nil
	}
}

func parseCreateArgs(args []string) (title string, jsonOut bool, err error) {
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", false, fmt.Errorf("unsupported create flag %q", arg)
		}
		if title == "" {
			title = arg
			continue
		}
		title += " " + arg
	}
	if strings.TrimSpace(title) == "" {
		return "", false, fmt.Errorf("bd create: missing title")
	}
	return title, jsonOut, nil
}

func parseShowArgs(args []string) (id string, jsonOut bool, err error) {
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", false, fmt.Errorf("unsupported show flag %q", arg)
		}
		if id == "" {
			id = arg
			continue
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("bd show: missing bead id")
	}
	return id, jsonOut, nil
}

func parseCloseArgs(args []string) (id string, jsonOut bool, err error) {
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return "", false, fmt.Errorf("unsupported close flag %q", arg)
		}
		if id == "" {
			id = arg
			continue
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("bd close: missing bead id")
	}
	return id, jsonOut, nil
}

func parseListArgs(args []string) (beads.ListQuery, bool, error) {
	q := beads.ListQuery{AllowScan: true}
	jsonOut := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			jsonOut = true
		case arg == "--all":
			q.IncludeClosed = true
		case arg == "--unassigned":
			q.Assignee = ""
		case strings.HasPrefix(arg, "--assignee="):
			q.Assignee = strings.TrimPrefix(arg, "--assignee=")
		case arg == "--assignee" && i+1 < len(args):
			i++
			q.Assignee = args[i]
		case strings.HasPrefix(arg, "--status="):
			q.Status = strings.TrimPrefix(arg, "--status=")
		case arg == "--status" && i+1 < len(args):
			i++
			q.Status = args[i]
		case strings.HasPrefix(arg, "--label="):
			q.Label = strings.TrimPrefix(arg, "--label=")
		case arg == "--label" && i+1 < len(args):
			i++
			q.Label = args[i]
		case strings.HasPrefix(arg, "--metadata-field="):
			key, value, ok := strings.Cut(strings.TrimPrefix(arg, "--metadata-field="), "=")
			if !ok || key == "" {
				return q, jsonOut, fmt.Errorf("invalid metadata-field %q", arg)
			}
			if q.Metadata == nil {
				q.Metadata = map[string]string{}
			}
			q.Metadata[key] = value
		case arg == "--metadata-field" && i+1 < len(args):
			i++
			key, value, ok := strings.Cut(args[i], "=")
			if !ok || key == "" {
				return q, jsonOut, fmt.Errorf("invalid metadata-field %q", args[i])
			}
			if q.Metadata == nil {
				q.Metadata = map[string]string{}
			}
			q.Metadata[key] = value
		case strings.HasPrefix(arg, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				return q, jsonOut, err
			}
			q.Limit = n
		case arg == "--limit" && i+1 < len(args):
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return q, jsonOut, err
			}
			q.Limit = n
		case strings.HasPrefix(arg, "-"):
			return q, jsonOut, fmt.Errorf("unsupported list flag %q", arg)
		}
	}
	return q, jsonOut, nil
}

func parseReadyArgs(args []string) (beads.ListQuery, bool, error) {
	q, jsonOut, err := parseListArgs(args)
	if err != nil {
		return q, jsonOut, err
	}
	q.AllowScan = true
	q.IncludeClosed = false
	if q.Status == "" {
		q.Status = "open"
	}
	return q, jsonOut, nil
}

func parseUpdateArgs(args []string) (id string, opts beads.UpdateOpts, jsonOut bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			jsonOut = true
		case arg == "--claim":
			assignee := actor()
			opts.Assignee = &assignee
			status := "in_progress"
			opts.Status = &status
		case strings.HasPrefix(arg, "--assignee="):
			assignee := strings.TrimPrefix(arg, "--assignee=")
			opts.Assignee = &assignee
		case arg == "--assignee" && i+1 < len(args):
			i++
			assignee := args[i]
			opts.Assignee = &assignee
		case strings.HasPrefix(arg, "--status="):
			status := strings.TrimPrefix(arg, "--status=")
			opts.Status = &status
		case arg == "--status" && i+1 < len(args):
			i++
			status := args[i]
			opts.Status = &status
		case strings.HasPrefix(arg, "--set-metadata="):
			key, value, ok := strings.Cut(strings.TrimPrefix(arg, "--set-metadata="), "=")
			if !ok || key == "" {
				return "", opts, false, fmt.Errorf("invalid metadata %q", arg)
			}
			if opts.Metadata == nil {
				opts.Metadata = map[string]string{}
			}
			opts.Metadata[key] = value
		case arg == "--set-metadata" && i+1 < len(args):
			i++
			key, value, ok := strings.Cut(args[i], "=")
			if !ok || key == "" {
				return "", opts, false, fmt.Errorf("invalid metadata %q", args[i])
			}
			if opts.Metadata == nil {
				opts.Metadata = map[string]string{}
			}
			opts.Metadata[key] = value
		case strings.HasPrefix(arg, "-"):
			return "", opts, false, fmt.Errorf("unsupported update flag %q", arg)
		case id == "":
			id = arg
		default:
			return "", opts, false, fmt.Errorf("unexpected update arg %q", arg)
		}
	}
	if id == "" {
		return "", opts, false, fmt.Errorf("bd update: missing bead id")
	}
	return id, opts, jsonOut, nil
}

func openFileStore(cityDir string) (beads.Store, *events.FileRecorder, error) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityDir, ".gc", "beads.json"))
	if err != nil {
		return nil, nil, err
	}
	store.SetLocker(beads.NewFileFlock(filepath.Join(cityDir, ".gc", "beads.json.lock")))
	recorder, err := events.NewFileRecorder(filepath.Join(cityDir, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return nil, nil, err
	}
	return store, recorder, nil
}

func actor() string {
	for _, key := range []string{"BEADS_ACTOR", "GC_SESSION_NAME", "GC_AGENT"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "human"
}

func record(recorder *events.FileRecorder, eventType string, actorName, subject, message string) {
	recorder.Record(events.Event{
		Type:    eventType,
		Actor:   actorName,
		Subject: subject,
		Message: message,
	})
}

func writeBead(stdout io.Writer, b beads.Bead, jsonOut bool, created bool) error {
	if jsonOut {
		return json.NewEncoder(stdout).Encode(beadWireMap(b))
	}
	if created {
		_, err := fmt.Fprintf(stdout, "Created bead: %s\n", b.ID)
		return err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "ID: %s\n", b.ID)
	fmt.Fprintf(&sb, "Title: %s\n", b.Title)
	fmt.Fprintf(&sb, "Status: %s\n", b.Status)
	if b.Assignee != "" {
		fmt.Fprintf(&sb, "Assignee: %s\n", b.Assignee)
	}
	_, err := io.WriteString(stdout, sb.String())
	return err
}

func writeList(stdout io.Writer, items []beads.Bead, jsonOut bool) error {
	if jsonOut {
		out := make([]map[string]any, 0, len(items))
		for _, b := range items {
			out = append(out, beadWireMap(b))
		}
		return json.NewEncoder(stdout).Encode(out)
	}
	if len(items) == 0 {
		_, err := fmt.Fprintln(stdout, "No beads.")
		return err
	}
	for _, b := range items {
		if _, err := fmt.Fprintf(stdout, "%s  %s  %s\n", b.ID, b.Status, b.Title); err != nil {
			return err
		}
	}
	return nil
}

func beadWireMap(b beads.Bead) map[string]any {
	return map[string]any{
		"id":         b.ID,
		"title":      b.Title,
		"status":     b.Status,
		"assignee":   b.Assignee,
		"type":       b.Type,
		"created_at": b.CreatedAt,
		"updated_at": b.CreatedAt,
		"metadata":   b.Metadata,
	}
}
