package agentutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRoot returns the repository root by navigating from this file's location.
func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// TestRoutedToIdentityDerivationIsCentralized enforces that every gc.routed_to
// derivation (PoolName-first, falling back to QualifiedName()) flows through
// RoutedToIdentity. Reimplementing the check inline lets a pool instance's
// routing diverge from what gc sling stamped for its base template, making
// the bead invisible to pool demand/claim — this has independently regressed
// at multiple call sites (see ga-79uuwq, ga-s635qm).
//
// Not violations (allowed):
//   - internal/agentutil/resolve.go — RoutedToIdentity's own implementation.
//   - internal/config/workquery.go — poolDemandTarget, DefaultSlingQuery,
//     effectiveOnDeath, and effectiveOnBoot must inline the check: agentutil
//     imports config, so config cannot call back into agentutil without an
//     import cycle. This is a reviewed, permanent exception, not a pending
//     migration.
func TestRoutedToIdentityDerivationIsCentralized(t *testing.T) {
	violations, err := routedToIdentityViolations(repoRoot())
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("inline PoolName collapse found outside RoutedToIdentity (%d violations):", len(violations))
		for _, violation := range violations {
			t.Errorf("  %s", violation)
		}
		t.Error("Call agentutil.RoutedToIdentity(agent) instead of reimplementing the PoolName-first collapse.")
	}
}

func routedToIdentityViolations(root string) ([]string, error) {
	allowedFiles := []string{
		filepath.Join("internal", "agentutil", "resolve.go"),
		filepath.Join("internal", "config", "workquery.go"),
	}

	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "vendor" || base == ".claude" || base == ".gc" || strings.HasPrefix(base, ".beads-src") {
				return filepath.SkipDir
			}
			// Skip git worktrees embedded in the repo (have a .git file, not
			// dir) — but never apply this to root itself. gc builder/reviewer/
			// deployer sessions run from inside a worktree, so root legitimately
			// has a .git file rather than a .git directory; skipping on that
			// condition here would SkipDir the walk's very first entry and
			// silently visit zero files.
			if path != root {
				if fi, serr := os.Stat(filepath.Join(path, ".git")); serr == nil && !fi.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, allowed := range allowedFiles {
			if rel == allowed {
				return nil
			}
		}

		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, path, nil, 0)
		if err != nil {
			return err
		}
		refusalChecks := make(map[*ast.BinaryExpr]bool)
		ast.Inspect(file, func(node ast.Node) bool {
			if branch, ok := node.(*ast.IfStmt); ok && isTerminalPoolRefusal(branch, file, path) {
				markDirectRefusalChecks(branch.Cond, refusalChecks)
			}
			if comparison, ok := node.(*ast.BinaryExpr); ok && comparesPoolNameToEmpty(comparison) && !refusalChecks[comparison] {
				violations = append(violations, rel+":"+itoa(positions.Position(comparison.Pos()).Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(violations)
	return violations, nil
}

func TestRoutingPolicyRejectsSideEffectingErrorConstructor(t *testing.T) {
	root := t.TempDir()
	source := `package fixture
type Agent struct { PoolName string }
type Result struct{}
var route string
var selectedAgent Agent
func pretendRefusal(string) error {
    route = selectedAgent.PoolName
    return nil
}
func selectTarget(a Agent) (Result, error) {
    if a.PoolName != "" {
        return Result{}, pretendRefusal("blocked")
    }
    return Result{}, nil
}`
	if err := os.WriteFile(filepath.Join(root, "policy.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	violations, err := routedToIdentityViolations(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("routing gate accepted identity selection hidden in a supposed error constructor")
	}
}

func TestRoutingPolicyClassifiesRefusalsWithoutExemptingIdentitySelection(t *testing.T) {
	const prefix = `package fixture
import "fmt"
type Agent struct { PoolName string; Dir string }
func (Agent) QualifiedName() string { return "rig/worker" }
type Result struct{}
var route string
var selectedAgent Agent
var _ = fmt.Errorf
func RoutedToIdentity(*Agent) string { return "rig/worker" }
type formatter string
func (formatter) Errorf(string) error { route = selectedAgent.PoolName; return nil }
type routingStringer struct{}
func (routingStringer) String() string { route = selectedAgent.PoolName; return "blocked" }
var trigger routingStringer
`
	const pureConstructor = `func refusal(reason string) error { return fmt.Errorf("refused: %s", reason) }
`
	for _, tc := range []struct {
		name, body, constructor string
		allowed                 bool
	}{
		{
			name: "terminal membership refusal", allowed: true,
			body: `if a.PoolName != "" { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name: "compound admission refusal", allowed: true,
			body: `canonical := RoutedToIdentity(&a); if a.PoolName != "" || canonical != a.QualifiedName() || a.Dir == "" { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name: "central helper only", allowed: true,
			body: `route = RoutedToIdentity(&a); return Result{}, nil`,
		},
		{
			name: "inline assignment fallback",
			body: "route = a.QualifiedName(); if a.PoolName != \"\" { route = a.PoolName }; return Result{}, nil",
		},
		{
			name: "helper does not waive later fallback",
			body: `route = RoutedToIdentity(&a); if a.PoolName != "" { route = a.PoolName }; return Result{}, nil`,
		},
		{
			name: "assignment before error return",
			body: `if a.PoolName != "" { route = a.PoolName; return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name: "side effecting condition closure",
			body: `if a.PoolName != "" && func() bool { route = a.PoolName; return true }() { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name: "nested comparison closure",
			body: `if func() bool { if a.PoolName != "" { route = a.PoolName; return true }; return false }() { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name: "bare alias",
			body: `PoolName := a.PoolName; if PoolName != "" { route = PoolName }; return Result{}, nil`,
		},
		{
			name: "parenthesized raw empty comparison",
			body: "if ((a.PoolName)) != (``) { route = a.PoolName }; return Result{}, nil",
		},
		{
			name: "else branch cannot be exempted",
			body: `if a.PoolName != "" { return Result{}, refusal("blocked") } else { route = a.QualifiedName() }; return Result{}, nil`,
		},
		{
			name:        "constructor returning nil",
			constructor: "func refusal(string) error { return nil }\n",
			body:        `if a.PoolName != "" { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name:        "shadowed error formatter",
			constructor: "func refusal(fmt formatter) error { return fmt.Errorf(\"blocked\") }\n",
			body:        `if a.PoolName != "" { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
		{
			name:        "formatter invokes side effecting stringer",
			constructor: "func refusal(string) error { return fmt.Errorf(\"%s\", trigger) }\n",
			body:        `if a.PoolName != "" { return Result{}, refusal("blocked") }; return Result{}, nil`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			constructor := tc.constructor
			if constructor == "" {
				constructor = pureConstructor
			}
			source := prefix + constructor + "func selectTarget(a Agent) (Result, error) { " + tc.body + " }\n"
			if err := os.WriteFile(filepath.Join(root, "policy.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			violations, err := routedToIdentityViolations(root)
			if err != nil {
				t.Fatal(err)
			}
			if (len(violations) == 0) != tc.allowed {
				t.Fatalf("routing policy violations = %v, want allowed=%t", violations, tc.allowed)
			}
		})
	}
}

func TestRoutingPolicyWalksWorktreeRootAndRejectsMalformedSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /unused/test-owned-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "policy.go")
	if err := os.WriteFile(path, []byte(`package fixture; func route(a Agent) { if a.PoolName != "" { selected = a.PoolName } }`), 0o600); err != nil {
		t.Fatal(err)
	}
	violations, err := routedToIdentityViolations(root)
	if err != nil || len(violations) == 0 {
		t.Fatalf("worktree root was not inspected: violations=%v err=%v", violations, err)
	}
	if err := os.WriteFile(path, []byte("package fixture; func malformed("), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := routedToIdentityViolations(root); err == nil {
		t.Fatal("routing policy accepted a source parse failure")
	}
}

func TestRoutingPolicyAllowsOnlyClosedSentinelReferences(t *testing.T) {
	const source = `package fixture
import ("errors"; "fmt")
type Agent struct { PoolName string }
type Result struct{}
var route string
var selectedAgent Agent
var sentinel = errors.New("blocked")
type trigger struct{}
func (trigger) Error() string { route = selectedAgent.PoolName; return "blocked" }
func refusal(reason string) error { return fmt.Errorf("%w: %s", sentinel, reason) }
func selectTarget(a Agent) (Result, error) {
    if a.PoolName != "" { return Result{}, refusal("blocked") }
    return Result{}, nil
}`
	for _, tc := range []struct {
		name, peer string
		allowed    bool
	}{
		{name: "closed literal sentinel", allowed: true},
		{name: "cross file reassignment", peer: `package fixture; func replace() { sentinel = trigger{} }`},
		{name: "address escape", peer: `package fixture; var escaped = &sentinel`},
		{name: "package builtin shadow", peer: `package fixture; type string struct{}`},
		{name: "unparseable peer", peer: `package fixture; func malformed(`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "policy.go"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.peer != "" {
				if err := os.WriteFile(filepath.Join(root, "peer_test.go"), []byte(tc.peer), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			violations, err := routedToIdentityViolations(root)
			if err != nil {
				t.Fatal(err)
			}
			if (len(violations) == 0) != tc.allowed {
				t.Fatalf("routing policy violations = %v, want allowed=%t", violations, tc.allowed)
			}
		})
	}
}

// Only direct Boolean predicates belong to a terminal refusal. Keep walking
// calls and function literals normally, so an inline routing derivation hidden
// inside a condition is still rejected.
func markDirectRefusalChecks(expr ast.Expr, checks map[*ast.BinaryExpr]bool) {
	switch expr := expr.(type) {
	case *ast.ParenExpr:
		markDirectRefusalChecks(expr.X, checks)
	case *ast.UnaryExpr:
		if expr.Op == token.NOT {
			markDirectRefusalChecks(expr.X, checks)
		}
	case *ast.BinaryExpr:
		if comparesPoolNameToEmpty(expr) {
			checks[expr] = true
		} else if expr.Op == token.LAND || expr.Op == token.LOR {
			markDirectRefusalChecks(expr.X, checks)
			markDirectRefusalChecks(expr.Y, checks)
		}
	}
}

// A refusal does not derive an identity: it returns only a zero struct result
// and a literal-argument, locally declared error constructor. Do not exempt the surrounding file/function or
// a branch with an initializer, else, assignment, or identity-bearing return.
func isTerminalPoolRefusal(branch *ast.IfStmt, file *ast.File, sourcePath string) bool {
	if branch.Init != nil || branch.Else != nil || len(branch.Body.List) != 1 {
		return false
	}
	checks := make(map[*ast.BinaryExpr]bool)
	markDirectRefusalChecks(branch.Cond, checks)
	if len(checks) == 0 {
		return false
	}
	safeCondition := true
	ast.Inspect(branch.Cond, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			safeCondition = false
			return false
		case *ast.BinaryExpr:
			if checks[node] {
				// A call/closure as the selector's receiver can itself select an
				// identity. Only a plain member or bare alias is a membership test.
				for _, operand := range []ast.Expr{unparenRoutingExpr(node.X), unparenRoutingExpr(node.Y)} {
					if member, ok := operand.(*ast.SelectorExpr); ok {
						if _, ok := member.X.(*ast.Ident); !ok {
							safeCondition = false
						}
					}
				}
				return false
			}
		case *ast.SelectorExpr:
			if node.Sel.Name == "PoolName" {
				safeCondition = false
			}
		case *ast.Ident:
			if node.Name == "PoolName" {
				safeCondition = false
			}
		}
		return true
	})
	if !safeCondition {
		return false
	}
	ret, ok := branch.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 2 {
		return false
	}
	zero, ok := ret.Results[0].(*ast.CompositeLit)
	if !ok || len(zero.Elts) != 0 {
		return false
	}
	errCall, ok := ret.Results[1].(*ast.CallExpr)
	if !ok || len(errCall.Args) == 0 {
		return false
	}
	for _, arg := range errCall.Args {
		literal, ok := arg.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return false
		}
	}
	callee, ok := errCall.Fun.(*ast.Ident)
	if !ok || callee.Obj == nil || callee.Obj.Kind != ast.Fun {
		return false
	}
	constructor, ok := callee.Obj.Decl.(*ast.FuncDecl)
	if !ok || constructor.Type.Results == nil || len(constructor.Type.Results.List) != 1 {
		return false
	}
	errorType, ok := constructor.Type.Results.List[0].Type.(*ast.Ident)
	if !ok || errorType.Name != "error" || errorType.Obj != nil || constructor.Body == nil || len(constructor.Body.List) != 1 {
		return false
	}
	constructorReturn, ok := constructor.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(constructorReturn.Results) != 1 {
		return false
	}
	formatError, ok := constructorReturn.Results[0].(*ast.CallExpr)
	if !ok || len(formatError.Args) == 0 || formatError.Ellipsis.IsValid() {
		return false
	}
	format, ok := formatError.Args[0].(*ast.BasicLit)
	if !ok || format.Kind != token.STRING {
		return false
	}
	method, ok := formatError.Fun.(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "Errorf" {
		return false
	}
	pkg, ok := method.X.(*ast.Ident)
	if !ok || pkg.Obj != nil {
		return false
	}
	importedFmt := false
	for _, imported := range file.Imports {
		if imported.Path.Value != `"fmt"` {
			continue
		}
		name := "fmt"
		if imported.Name != nil {
			name = imported.Name.Name
		}
		importedFmt = name == pkg.Name
	}
	if !importedFmt {
		return false
	}
	packageFiles, ok := routingPackageFiles(file, sourcePath)
	if !ok {
		return false
	}
	// Formatting invokes user methods on arbitrary values. Accept only literal
	// strings, builtin string parameters, and a closed literal error sentinel.
	for _, arg := range formatError.Args[1:] {
		switch arg := arg.(type) {
		case *ast.BasicLit:
			if arg.Kind != token.STRING {
				return false
			}
		case *ast.Ident:
			if !isRoutingStringParameter(arg, constructor) && !isClosedRoutingErrorSentinel(arg, formatError, file, packageFiles) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isRoutingStringParameter(operand *ast.Ident, constructor *ast.FuncDecl) bool {
	if operand.Obj == nil || constructor.Type.Params == nil {
		return false
	}
	for _, parameter := range constructor.Type.Params.List {
		kind, ok := parameter.Type.(*ast.Ident)
		if ok && kind.Name == "string" && kind.Obj == nil && operand.Obj.Decl == parameter {
			return true
		}
	}
	return false
}

// A sentinel is safe only in the closed declaration/formatting seam. Inspect
// every same-package source (including tests and build variants) and refuse
// any other reference, rather than trying to infer writes or address escapes.
func isClosedRoutingErrorSentinel(operand *ast.Ident, formatting *ast.CallExpr, file *ast.File, packageFiles []*ast.File) bool {
	if operand.Obj == nil || operand.Obj.Kind != ast.Var || ast.IsExported(operand.Name) || file.Scope.Lookup(operand.Name) != operand.Obj {
		return false
	}
	declaration, ok := operand.Obj.Decl.(*ast.ValueSpec)
	if !ok || declaration.Type != nil || len(declaration.Names) != 1 || len(declaration.Values) != 1 {
		return false
	}
	initializer, ok := declaration.Values[0].(*ast.CallExpr)
	if !ok || initializer.Ellipsis.IsValid() || len(initializer.Args) != 1 {
		return false
	}
	message, ok := initializer.Args[0].(*ast.BasicLit)
	if !ok || message.Kind != token.STRING {
		return false
	}
	method, ok := initializer.Fun.(*ast.SelectorExpr)
	if !ok || method.Sel.Name != "New" {
		return false
	}
	pkg, ok := method.X.(*ast.Ident)
	if !ok || pkg.Obj != nil {
		return false
	}
	importedErrors := false
	for _, imported := range file.Imports {
		if imported.Path.Value == `"errors"` {
			name := "errors"
			if imported.Name != nil {
				name = imported.Name.Name
			}
			importedErrors = name == pkg.Name
		}
	}
	if !importedErrors {
		return false
	}
	allowed := map[*ast.Ident]bool{declaration.Names[0]: true}
	for _, argument := range formatting.Args {
		if identifier, ok := argument.(*ast.Ident); ok && identifier.Obj == operand.Obj {
			allowed[identifier] = true
		}
	}
	for _, candidate := range packageFiles {
		safe := true
		ast.Inspect(candidate, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok && identifier.Name == operand.Name && !allowed[identifier] {
				safe = false
			}
			return true
		})
		if !safe {
			return false
		}
	}
	return true
}

func routingPackageFiles(file *ast.File, sourcePath string) ([]*ast.File, bool) {
	entries, err := os.ReadDir(filepath.Dir(sourcePath))
	if err != nil {
		return nil, false
	}
	var files []*ast.File
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(filepath.Dir(sourcePath), entry.Name())
		candidate := file
		if path != sourcePath {
			candidate, err = parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return nil, false
			}
		}
		if candidate.Name.Name != file.Name.Name {
			continue
		}
		if candidate.Scope.Lookup("string") != nil || candidate.Scope.Lookup("error") != nil {
			return nil, false
		}
		files = append(files, candidate)
	}
	return files, true
}

func unparenRoutingExpr(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

func isRoutingPoolName(expr ast.Expr) bool {
	switch expr := unparenRoutingExpr(expr).(type) {
	case *ast.Ident:
		return expr.Name == "PoolName"
	case *ast.SelectorExpr:
		return expr.Sel.Name == "PoolName"
	default:
		return false
	}
}

func comparesPoolNameToEmpty(comparison *ast.BinaryExpr) bool {
	if comparison.Op != token.NEQ && comparison.Op != token.EQL {
		return false
	}
	isEmpty := func(expr ast.Expr) bool {
		literal, ok := unparenRoutingExpr(expr).(*ast.BasicLit)
		return ok && literal.Kind == token.STRING && (literal.Value == `""` || literal.Value == "``")
	}
	return (isRoutingPoolName(comparison.X) && isEmpty(comparison.Y)) || (isEmpty(comparison.X) && isRoutingPoolName(comparison.Y))
}

// itoa converts an int to a string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
