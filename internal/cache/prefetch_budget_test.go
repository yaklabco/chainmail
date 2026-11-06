package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
)

// leave spends today's budget down to left bytes.
func (r *prefetchRig) leave(left int64) {
	r.t.Helper()
	if _, err := r.st.AddBudget(r.t.Context(), "acme", utcDay(r.clk.Now()), r.set.DailyDownload-left); err != nil {
		r.t.Fatal(err)
	}
}

func (r *prefetchRig) budgetOn(day string) int64 {
	r.t.Helper()
	n, err := r.st.Budget(r.t.Context(), "acme", day)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// rawLen is len(raw) of x's stored body.
func (r *prefetchRig) rawLen(x ref.Ref) int64 {
	r.t.Helper()
	body, ok, err := r.st.Body(r.t.Context(), x)
	if err != nil || !ok {
		r.t.Fatalf("Body(%v) = %v, %v", x, ok, err)
	}
	return int64(len(body.Raw))
}

// failureReason is the reason recorded for x's body failure, "" for none.
func (r *prefetchRig) failureReason(x ref.Ref) string {
	r.t.Helper()
	var reason string
	err := r.st.db.QueryRowContext(r.t.Context(), "SELECT reason FROM body_failures WHERE msg = ?", r.msgID(x)).Scan(&reason)
	if err != nil {
		return ""
	}
	return reason
}

// afterFetch hands out the fake's bodies and runs after once the first
// FetchMessages call has answered and before it returns, as a foreground
// read or the builder spending budget, or the day turning, meanwhile.
type afterFetch struct {
	*fake.Store
	once  *sync.Once
	after func()
}

func (a afterFetch) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out := a.Store.FetchMessages(ctx, refs)
	a.once.Do(a.after)
	return out
}

// With one byte of the day's budget left, no body is admitted: the
// prefetcher fetches nothing, records nothing, and waits for 00:00 UTC.
func TestPrefetchOneByteLeftAdmitsNothing(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "m")
	r.track(true)
	r.leave(1)
	r.step(untilUTCMidnight(r.clk.Now()))
	if r.fetches() != 0 || r.has(m) {
		t.Errorf("FetchMessages calls = %d, body cached = %v; want none", r.fetches(), r.has(m))
	}
	if !r.offered(m) {
		t.Error("the body is not offered again: a failure was recorded for a budget wait")
	}
}

// A step admits bodies, newest first, only while their stored sizes add up
// to no more than the day's remaining budget. The next step admits again
// against what the downloads really spent.
func TestPrefetchAdmitsOnlyWhatFits(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	const size = 10_000
	newest := r.addSized(oneDay, "newest", size)
	middle := r.addSized(2*oneDay, "middle", size)
	oldest := r.addSized(3*oneDay, "oldest", size)
	r.track(true)
	r.leave(2*size + size/2)
	r.step(0)
	if !r.has(newest) || !r.has(middle) || r.has(oldest) {
		t.Errorf("bodies cached: newest %v, middle %v, oldest %v; want true, true, false", r.has(newest), r.has(middle), r.has(oldest))
	}
	if r.fetches() != 2 {
		t.Errorf("FetchMessages calls = %d, want 2", r.fetches())
	}
	r.step(0)
	if !r.has(oldest) {
		t.Error("the oldest body was not fetched once the real spend left room for it")
	}
}

// A body larger than the day's remaining budget waits for the next UTC
// day, and so do the smaller ones behind it.
func TestPrefetchBodyLargerThanTheRestWaitsForTheNextDay(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	big := r.addSized(oneDay, "big", 10_000)
	small := r.addSized(2*oneDay, "small", 100)
	r.track(true)
	r.leave(5_000)
	r.step(untilUTCMidnight(r.clk.Now()))
	if r.fetches() != 0 || r.has(big) || r.has(small) {
		t.Errorf("FetchMessages calls = %d, bodies cached big %v small %v; want none", r.fetches(), r.has(big), r.has(small))
	}
	r.clk.Set(time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC))
	r.step(0)
	if !r.has(big) || !r.has(small) {
		t.Errorf("on the next day: bodies cached big %v small %v; want both", r.has(big), r.has(small))
	}
}

// A body larger than the whole daily budget can never be admitted. It is
// recorded as a failure, too_large, rather than holding up the bodies
// behind it, and is final after the usual attempts, which completes the
// window.
func TestPrefetchBodyLargerThanTheDayIsGivenUp(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	r.set.DailyDownload = 16 << 20
	huge := r.addSized(oneDay, "huge", 16<<20+1)
	small := r.addSized(2*oneDay, "small", 100)
	r.track(true)
	r.step(0)
	if r.has(huge) || !r.has(small) || r.fetches() != 1 {
		t.Errorf("bodies cached huge %v small %v, FetchMessages calls %d; want false, true, 1", r.has(huge), r.has(small), r.fetches())
	}
	if got := r.failureReason(huge); got != failTooLarge {
		t.Errorf("huge failure reason = %q, want %q", got, failTooLarge)
	}
	for range maxBodyAttempts - 1 {
		r.clk.Advance(bodyRetryGap + time.Second)
		r.step(0)
	}
	r.step(prefetchIdle)
	if r.fetches() != 1 || !r.windowDone() || r.finalFailures() != 1 {
		t.Errorf("calls = %d, window_done = %v, final failures = %d; want 1, true, 1", r.fetches(), r.windowDone(), r.finalFailures())
	}
}

// The budget is checked again before every download in a step: when
// foreground reads or the builder spend the rest of the day's budget
// during a download, the step fetches no further body and waits for
// 00:00 UTC.
func TestPrefetchRechecksTheBudgetBetweenDownloads(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	first := r.add(oneDay, "first")
	second := r.add(2*oneDay, "second")
	r.track(true)
	r.mail = afterFetch{Store: r.fk, once: &sync.Once{}, after: func() {
		if _, err := r.st.AddBudget(context.Background(), "acme", utcDay(r.clk.Now()), r.set.DailyDownload); err != nil {
			t.Error(err)
		}
	}}
	r.step(untilUTCMidnight(r.clk.Now()))
	if !r.has(first) || r.has(second) || r.fetches() != 1 {
		t.Errorf("bodies cached first %v second %v, FetchMessages calls %d; want true, false, 1", r.has(first), r.has(second), r.fetches())
	}
}

// A download is charged to the UTC day it ends on, not the one it started
// on.
func TestPrefetchChargesTheDayTheFetchEnds(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "m")
	r.track(true)
	r.clk.Set(time.Date(2026, 11, 3, 23, 59, 59, 0, time.UTC))
	r.mail = afterFetch{Store: r.fk, once: &sync.Once{}, after: func() {
		r.clk.Set(time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC))
	}}
	r.step(0)
	if got := r.budgetOn("2026-11-03"); got != 0 {
		t.Errorf("budget used on 2026-11-03 = %d, want 0", got)
	}
	if got, want := r.budgetOn("2026-11-04"), r.rawLen(m); got != want {
		t.Errorf("budget used on 2026-11-04 = %d, want %d", got, want)
	}
}

// After ErrUnavailable the prefetcher waits 1s doubling to 5m, as the
// other workers do, and starts again from 1s once a download succeeds.
func TestPrefetchBacksOffWhileTheServerIsUnavailable(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	m := r.add(oneDay, "m")
	r.track(true)
	r.fk.Fail("FetchMessages", fmt.Errorf("%w: reset", mailstore.ErrUnavailable))
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		r.step(want)
	}
	for range 6 {
		r.p.step(t.Context())
	}
	r.step(workRetryMax)
	r.fk.Fail("FetchMessages", nil)
	r.step(0)
	if !r.has(m) {
		t.Fatal("body not fetched once the server came back")
	}
	r.add(2*oneDay, "next")
	r.fk.Fail("FetchMessages", fmt.Errorf("%w: reset", mailstore.ErrUnavailable))
	r.step(time.Second)
}

// A server throttling the account pauses the prefetcher until 00:00 UTC:
// no fetch is attempted meanwhile and no failure is recorded, and it
// resumes on the next day.
func TestPrefetchThrottledWaitsForTheNextUTCDay(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	r.p.o.Throttle = &throttle{}
	m := r.add(oneDay, "m")
	r.track(true)
	r.clk.Set(time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC))
	r.fk.Fail("FetchMessages", fmt.Errorf("%w: acme", mailstore.ErrThrottled))
	r.step(9 * time.Hour)
	if r.fetches() != 1 || r.has(m) || !r.offered(m) {
		t.Errorf("FetchMessages calls = %d, body cached %v, offered %v; want 1, false, true", r.fetches(), r.has(m), r.offered(m))
	}
	r.fk.Fail("FetchMessages", nil)
	r.clk.Set(time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC))
	r.step(time.Hour)
	if r.fetches() != 1 {
		t.Errorf("FetchMessages calls while throttled = %d, want 1", r.fetches())
	}
	r.clk.Set(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	r.step(0)
	if !r.has(m) {
		t.Error("body not fetched after 00:00 UTC")
	}
}

// spendTheRest hands out the fake's bodies and, on the first
// FetchMessages call, spends the day's budget so that charging that call
// leaves exactly 0 bytes: a foreground read meanwhile, sized to the byte.
type spendTheRest struct {
	*fake.Store
	r    *prefetchRig
	once *sync.Once
}

func (s spendTheRest) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	out := s.Store.FetchMessages(ctx, refs)
	s.once.Do(func() {
		var raw int64
		for _, res := range out {
			if res.Message != nil {
				raw += int64(len(res.Message.Raw))
			}
		}
		day := utcDay(s.r.clk.Now())
		used, err := s.r.st.Budget(ctx, "acme", day)
		if err != nil {
			s.r.t.Error(err)
			return
		}
		if _, err := s.r.st.AddBudget(ctx, "acme", day, s.r.set.DailyDownload-used-raw); err != nil {
			s.r.t.Error(err)
		}
	})
	return out
}

// With exactly 0 bytes left after a download, the recheck before the next
// one admits nothing, not even a body whose stored size is 0.
func TestPrefetchZeroLeftAdmitsNoEmptyBody(t *testing.T) {
	t.Parallel()
	r := newPrefetchRig(t)
	first := r.add(oneDay, "first")
	empty := r.addSized(2*oneDay, "empty", 0)
	r.track(true)
	r.mail = spendTheRest{Store: r.fk, r: r, once: &sync.Once{}}
	r.step(untilUTCMidnight(r.clk.Now()))
	if !r.has(first) || r.has(empty) || r.fetches() != 1 {
		t.Errorf("bodies cached first %v empty %v, FetchMessages calls %d; want true, false, 1", r.has(first), r.has(empty), r.fetches())
	}
	if got := r.budgetOn(utcDay(r.clk.Now())); got != r.set.DailyDownload {
		t.Errorf("budget used = %d, want exactly %d", got, r.set.DailyDownload)
	}
}
