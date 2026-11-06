package cache_test

import (
	"testing"

	"github.com/yaklabco/chainmail/internal/cache"
)

// TestCombineSource checks the source of an answer built from parts: the
// one source they share, mixed when they differ, and server when there are
// none.
func TestCombineSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{name: "none", in: nil, want: cache.SourceServer},
		{name: "all cache", in: []string{cache.SourceCache, cache.SourceCache}, want: cache.SourceCache},
		{name: "all server", in: []string{cache.SourceServer}, want: cache.SourceServer},
		{name: "both", in: []string{cache.SourceCache, cache.SourceServer}, want: cache.SourceMixed},
		{name: "empty parts are skipped", in: []string{"", cache.SourceCache, ""}, want: cache.SourceCache},
		{name: "mixed stays mixed", in: []string{cache.SourceMixed, cache.SourceCache}, want: cache.SourceMixed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cache.CombineSource(tt.in...); got != tt.want {
				t.Errorf("CombineSource(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
