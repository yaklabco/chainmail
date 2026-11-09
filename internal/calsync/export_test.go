package calsync

import (
	"context"

	"github.com/yaklabco/chainmail/internal/calcache"
)

// SetSeriesRead replaces how s reads a stored series, so a test can make
// that read fail. Call it before s runs a cycle.
func SetSeriesRead(s *Syncer, f func(ctx context.Context, account, calID, providerID string) ([]calcache.EventRow, error)) {
	s.series = f
}
