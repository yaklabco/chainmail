package daemon

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/cache"
)

// answerSources are the values a get or thread response's source takes,
// set after the response is capped.
var answerSources = []string{cache.SourceCache, cache.SourceServer, cache.SourceMixed}

// padTo returns a padding string of the length that brings size(pad) to at
// most limit and within a few bytes of it, where size grows with its length.
func padTo(t *testing.T, limit int, size func(ref string) int) string {
	t.Helper()
	n := limit
	for range 4 {
		over := size(strings.Repeat("x", n)) - limit
		if over == 0 {
			break
		}
		n -= over
	}
	ref := strings.Repeat("x", n)
	if s := size(ref); s > limit || s < limit-2 {
		t.Fatalf("padded size %d, want within 2 bytes under %d", s, limit)
	}
	return ref
}

// TestCapItemsReservesTheSource checks that a get response that only just
// fits before its source is set still fits once any source is set.
func TestCapItemsReservesTheSource(t *testing.T) {
	t.Parallel()
	build := func(ref string) *chainmailv1.GetResponse {
		return &chainmailv1.GetResponse{Items: []*chainmailv1.GetItem{{
			Ref:    "r",
			Result: &chainmailv1.GetItem_Message{Message: &chainmailv1.Message{Ref: ref}},
		}}}
	}
	ref := padTo(t, maxResponseBytes, func(ref string) int { return proto.Size(build(ref)) })
	for _, src := range answerSources {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			resp := build(ref)
			capItems(resp)
			resp.Source = src
			if s := proto.Size(resp); s > maxResponseBytes {
				t.Errorf("response with source %q is %d bytes, over %d", src, s, maxResponseBytes)
			}
		})
	}
}

// TestCapThreadReservesTheSource checks the same for a thread response.
func TestCapThreadReservesTheSource(t *testing.T) {
	t.Parallel()
	// The padding goes in Account, not Ref: a message the cap drops leaves its
	// ref in OmittedRefs. capThread counts Truncated in its base, so the
	// padded size does too.
	msg := func(pad string) *chainmailv1.Message { return &chainmailv1.Message{Ref: "r", Account: pad} }
	size := func(pad string) int {
		return proto.Size(&chainmailv1.GetThreadResponse{Truncated: true, Messages: []*chainmailv1.Message{msg(pad)}})
	}
	pad := padTo(t, maxResponseBytes, size)
	for _, src := range answerSources {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			resp := &chainmailv1.GetThreadResponse{}
			capThread(resp, []*chainmailv1.Message{msg(pad)}, nil)
			resp.Source = src
			if s := proto.Size(resp); s > maxResponseBytes {
				t.Errorf("response with source %q is %d bytes, over %d", src, s, maxResponseBytes)
			}
		})
	}
}

// TestLongestSourceCoversEverySource checks that the caps reserve enough
// bytes for whichever source the response ends up with.
func TestLongestSourceCoversEverySource(t *testing.T) {
	t.Parallel()
	for _, src := range answerSources {
		if len(src) > len(longestSource) {
			t.Errorf("source %q is longer than longestSource %q", src, longestSource)
		}
	}
}
