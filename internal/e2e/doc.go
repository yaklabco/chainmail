// Package e2e holds the end-to-end test of chainmail bootstrap. It builds
// chainmail with the e2e tag and runs bootstrap for real: it creates the
// daemon user and the client group, installs the service, adds a test CA to
// the system trust store, and removes all of it afterwards.
//
// Because it changes the host, the test builds only with the e2e tag and
// skips unless CI=true and CHAINMAIL_E2E_MUTATE_HOST=1 are both set. CI's
// e2e-linux and e2e-macos jobs run it with `go tool stave test:e2e` on
// throwaway GitHub runners. Never set those variables on a machine you use.
//
// The helpers the test uses to judge what it finds, such as the owner and
// mode checks against setup.Layout, have unit tests that run without the
// tag and change nothing.
package e2e
