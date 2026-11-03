package cache

import "context"

// ReadTestCatchUp leaves a's builder as a running one that has opened its
// group and applied the event log up to its tail, so Freshness can report
// a folder fresh in the read view tests, which never run the account.
func ReadTestCatchUp(ctx context.Context, a *Account) error {
	tail, err := a.o.Log.Tail(ctx)
	if err != nil {
		return err
	}
	a.applied.Store(tail)
	return nil
}
