package ctxtest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExpiring(t *testing.T) {
	t.Parallel()
	type key struct{}
	parent := context.WithValue(t.Context(), key{}, "v")
	expire := make(chan struct{})
	ctx := Expiring(parent, expire)
	timed, cancelTimed := context.WithTimeout(ctx, time.Hour)
	defer cancelTimed()
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	if ctx.Err() != nil || timed.Err() != nil || canceled.Err() != nil {
		t.Fatalf("ended before expire: %v, %v, %v", ctx.Err(), timed.Err(), canceled.Err())
	}
	if ctx.Value(key{}) != "v" || timed.Value(key{}) != "v" {
		t.Error("values from the parent are lost")
	}
	close(expire)
	<-timed.Done()
	<-canceled.Done()
	for name, c := range map[string]context.Context{"ctx": ctx, "WithTimeout child": timed, "WithCancel child": canceled} {
		if !errors.Is(c.Err(), context.DeadlineExceeded) {
			t.Errorf("%s: Err = %v, want DeadlineExceeded", name, c.Err())
		}
	}
}
