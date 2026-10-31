//go:build stave

package main

import (
	"errors"
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
	if err := sh.RunV("git", "diff", "--exit-code", "--", "gen"); err != nil {
		return err
	}
	untracked, err := sh.Output("git", "status", "--porcelain", "--", "gen")
	if err != nil {
		return err
	}
	if untracked != "" {
		return errors.New("generated files not committed:\n" + untracked)
	}
	return nil
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

// Live runs the smoke test against real accounts through the daemon at
// $CHAINMAIL_SOCKET. The write tests wait minutes at a time for Gmail's
// search to catch up, so a full run takes far longer than go test's default
// 10 minute timeout.
func (Test) Live() error {
	return sh.RunV("go", "test", "-tags", "live", "-count=1", "-timeout", "120m", "-v", "./internal/live/")
}

// Integration runs the integration suite: Dovecot in a Docker container and
// the fake Gmail server. The test binary starts and removes its own container.
func (Test) Integration() error {
	return sh.RunV("go", "test", "-race", "-tags", "integration", "-count=1", "-timeout", "15m",
		"-p", parallelism(), "-parallel", parallelism(), "./internal/mailstore/imapstore/")
}

// Build compiles bin/chainmail without cgo.
func Build() error {
	return sh.RunWithV(map[string]string{"CGO_ENABLED": "0"}, "",
		"go", "build", "-trimpath", "-ldflags", ldflags(), "-o", "bin/chainmail", "./cmd/chainmail")
}

// parallelism caps go test -p and -parallel at a quarter of the cores, as the
// machine rules require.
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
