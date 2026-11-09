package main

import (
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestMainEmbedsTimeZoneData checks that the binary carries its own time
// zone database, so [rules] timezone loads on hosts without zoneinfo. The
// import has no exported effect to observe, and LoadLocation tries the
// host's zoneinfo before the embedded copy, so the import itself is
// checked.
func TestMainEmbedsTimeZoneData(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if path == "time/tzdata" && imp.Name != nil && imp.Name.Name == "_" {
			return
		}
	}
	t.Error(`main.go does not import _ "time/tzdata"`)
}
