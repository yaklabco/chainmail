//go:build stave

package main

import (
	"runtime"
	"strconv"
	"strings"

	"github.com/yaklabco/stave/pkg/sh"
	"github.com/yaklabco/stave/pkg/st"
)

// Default runs the full local check.
var Default = Check

// Check runs everything CI runs: generated code check, lint, tests and build.
func Check() {
	st.SerialDeps(GenerateCheck, Lint, Test.Unit, Build)
}

// Generate regenerates Protobuf and Connect code into gen/.
func Generate() error {
	return sh.RunV("buf", "generate")
}

// GenerateCheck fails if gen/ is out of date with proto/.
func GenerateCheck() error {
	st.Deps(Generate)
	return sh.RunV("git", "diff", "--exit-code", "--", "gen")
}

// Lint runs buf lint and golangci-lint.
func Lint() error {
	if err := sh.RunV("buf", "lint"); err != nil {
		return err
	}
	return sh.RunV("golangci-lint", "run", "./...")
}

// Test groups the test suites.
type Test st.Namespace

// Unit runs unit and component tests with the race detector.
func (Test) Unit() error {
	return sh.RunV("go", "test", "-race", "-p", parallelism(), "./...")
}

// Live runs the smoke test against real accounts through the daemon at $CHAINMAIL_SOCKET.
func (Test) Live() error {
	return sh.RunV("go", "test", "-tags", "live", "-count=1", "-v", "./internal/live/")
}

// Build compiles bin/chainmail without cgo.
func Build() error {
	return sh.RunWithV(map[string]string{"CGO_ENABLED": "0"}, "",
		"go", "build", "-trimpath", "-ldflags", ldflags(), "-o", "bin/chainmail", "./cmd/chainmail")
}

// parallelism caps go test -p at a quarter of the cores, as the machine rules require.
func parallelism() string {
	return strconv.Itoa(max(1, runtime.NumCPU()/4))
}

func ldflags() string {
	v, err := sh.Output("git", "describe", "--tags", "--always", "--dirty")
	if err != nil || strings.TrimSpace(v) == "" {
		v = "dev"
	}
	return "-s -w -X github.com/jamesainslie/chainmail/internal/version.Version=" + strings.TrimSpace(v)
}
