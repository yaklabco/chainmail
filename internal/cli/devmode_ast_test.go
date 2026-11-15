package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// devModeAllowed are the only files, relative to the module root, that may
// set daemon.Options.DevMode. The daemon command sets it from --dev, and the
// test harness starts dev daemons.
var devModeAllowed = []string{
	"internal/cli/daemon.go",
	"internal/testutil/daemontest/daemontest.go",
}

// TestOnlyDaemonCmdSetsDevMode keeps dev mode behind the daemon command's
// explicit opt-in: no other code may build a daemon.Options with DevMode or
// assign a DevMode field.
func TestOnlyDaemonCmdSetsDevMode(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if slices.Contains(devModeAllowed, rel) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		checked++
		for _, v := range devModeSetters(fset, file) {
			t.Errorf("%s: sets DevMode outside internal/cli/daemon.go", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("checked no Go files: the module root walk is broken")
	}
}

// The checker itself finds each way of setting DevMode, and nothing else.
func TestDevModeSettersFindsViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src string
		want      int
	}{
		{name: "daemon.Options literal", src: "package x\nvar _ = daemon.Options{DevMode: true}", want: 1},
		{name: "pointer literal", src: "package x\nvar _ = &daemon.Options{Logger: nil, DevMode: true}", want: 1},
		{name: "Options inside package daemon", src: "package daemon\nvar _ = Options{DevMode: true}", want: 1},
		{name: "assignment", src: "package x\nfunc f(o *daemon.Options) { o.DevMode = true }", want: 1},
		{name: "nested assignment", src: "package x\nfunc f(s *server) { s.opts.DevMode = false }", want: 1},
		{name: "reading DevMode", src: "package x\nfunc f(o daemon.Options) bool { return o.DevMode }", want: 0},
		{name: "another type's DevMode", src: "package x\nvar _ = v1.GetStatusResponse{DevMode: true}", want: 0},
		{name: "Options outside package daemon", src: "package x\nvar _ = Options{DevMode: true}", want: 0},
		{name: "aliased import", src: "package x\nimport d \"github.com/yaklabco/chainmail/internal/daemon\"\nvar _ = d.Options{DevMode: true}", want: 1},
		{name: "dot import", src: "package x\nimport . \"github.com/yaklabco/chainmail/internal/daemon\"\nvar _ = Options{DevMode: true}", want: 1},
		{name: "alias of another package", src: "package x\nimport d \"example.com/other\"\nvar _ = d.Options{DevMode: true}", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := devModeSetters(fset, file); len(got) != tt.want {
				t.Errorf("found %v, want %d", got, tt.want)
			}
		})
	}
}

// devModeSetters returns the position of every daemon.Options literal with
// a DevMode key, and every assignment to a DevMode field, in file.
func devModeSetters(fset *token.FileSet, file *ast.File) []string {
	names, dot := daemonImportNames(file)
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if !isDaemonOptions(n.Type, names, dot || file.Name.Name == "daemon") {
				return true
			}
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "DevMode" {
					found = append(found, fset.Position(kv.Pos()).String())
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "DevMode" {
					found = append(found, fset.Position(sel.Pos()).String())
				}
			}
		}
		return true
	})
	return found
}

// daemonImportPath is the package whose Options carries DevMode.
const daemonImportPath = "github.com/yaklabco/chainmail/internal/daemon"

// daemonImportNames returns the names file uses for the daemon package:
// "daemon" always, so a snippet without imports is still checked, plus any
// alias it is imported under. dot says it is dot-imported, so a bare
// Options means daemon.Options.
func daemonImportNames(file *ast.File) (names []string, dot bool) {
	names = []string{"daemon"}
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, "\"`") != daemonImportPath || imp.Name == nil {
			continue
		}
		switch imp.Name.Name {
		case ".":
			dot = true
		case "_":
		default:
			names = append(names, imp.Name.Name)
		}
	}
	return names, dot
}

// isDaemonOptions reports whether typ names the daemon package's Options:
// qualified by one of names, or bare when bare is true (the file is in
// package daemon or dot-imports it).
func isDaemonOptions(typ ast.Expr, names []string, bare bool) bool {
	switch typ := typ.(type) {
	case *ast.SelectorExpr:
		x, ok := typ.X.(*ast.Ident)
		return ok && slices.Contains(names, x.Name) && typ.Sel.Name == "Options"
	case *ast.Ident:
		return bare && typ.Name == "Options"
	}
	return false
}

// moduleRoot walks up from the test's directory to the one holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
