// Package live holds smoke tests that run against real accounts through a
// running daemon. Run them by hand with `go tool stave test:live`; never in CI.
// The rules the write tests share, such as when a send counts as delivered,
// need no daemon and have unit tests that run without the live tag. So does
// the organize test's folder cleanup, against an in-process daemon over fake
// stores.
package live
