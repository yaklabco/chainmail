package cache

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// TestViewReadThroughKicksTheEvictor checks that a body the read path
// stores is followed by the size check: the evictor is woken when the file
// is over max_size, and left alone when it is under, or when nothing new
// was stored.
func TestViewReadThroughKicksTheEvictor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		maxSize int64
		cached  bool
		kicks   int
	}{
		{name: "a stored body over the cap", maxSize: 1, kicks: 1},
		{name: "a stored body under the cap", maxSize: 1 << 40, kicks: 0},
		{name: "a hit stores nothing", maxSize: 1, cached: true, kicks: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			clk := newPrefetchClock(t10Start)
			st := openEvictStore(t, clk.Now)
			conn, err := db.OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			fk := fake.New("acme", false)
			fk.SetNow(clk.Now)
			r := fk.Add(fake.Message{Folder: "INBOX", Date: t10Start.Add(-oneDay), Subject: "m", Body: "text of m"})
			fs, err := fk.FolderState(ctx, "INBOX")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutProgress(ctx, Progress{
				Account: "acme", Folder: "INBOX", UIDValidity: fs.UIDValidity, BaselineUIDNext: fs.UIDNext,
				BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: t10Start,
			}); err != nil {
				t.Fatal(err)
			}
			if tt.cached {
				res := fk.FetchMessages(ctx, []ref.Ref{r})[0]
				if res.Err != nil {
					t.Fatal(res.Err)
				}
				if err := st.PutRows(ctx, []Row{messageRow(res.Message)}); err != nil {
					t.Fatal(err)
				}
				if _, err := st.PutBody(ctx, r, Body{Raw: res.Message.Raw}, false); err != nil {
					t.Fatal(err)
				}
			}
			a := NewAccount(AccountOptions{
				Account:  acctAccount(t, "hostinger", []string{"INBOX"}, nil),
				Settings: acctSettings,
				MaxSize:  func() int64 { return tt.maxSize },
				Store:    st,
				Log:      watch.NewLog(conn, clk.Now, nil),
				State:    watch.NewState(conn),
				Mail:     func(context.Context) (MailStore, func(), error) { return fk, func() {}, nil },
				Clock:    clk,
			})
			got, _ := a.View(fk).FetchMessagesFrom(ctx, []ref.Ref{r})
			if got[0].Err != nil {
				t.Fatal(got[0].Err)
			}
			if _, ok, err := st.Body(ctx, r); err != nil || !ok {
				t.Fatalf("body after the read: %v, %t", err, ok)
			}
			if n := len(st.evict.kickChan()); n != tt.kicks {
				t.Errorf("kicks queued = %d, want %d", n, tt.kicks)
			}
		})
	}
}
