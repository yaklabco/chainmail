package imapstore

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// emptyBaseline is a watch.Source that answers a baseline as if INBOX had
// been empty at UIDNEXT 1, and passes every later pass to sync, so the
// script server's messages all arrive as new mail.
type emptyBaseline struct {
	sync func(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error)
}

func (e emptyBaseline) Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	if req.From == (mailstore.Checkpoint{}) {
		st := mailstore.FolderState{UIDValidity: scriptUIDValidity, UIDNext: 1, HighestModSeq: scriptModSeq, CondStore: true}
		return mailstore.Delta{State: st, Rebased: true, UIDs: []uint32{}}, nil
	}
	return e.sync(ctx, req)
}

// A poll pass and a watch pass over new mail that holds a message the
// server sends malformed emit new events for the other messages, and the
// checkpoint moves past the bad one, batch after batch, even past a batch
// that holds nothing else.
func TestPassEmitsNewMailAroundAMalformedMessage(t *testing.T) {
	t.Parallel()
	for name, sync := range syncers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 4, condstore: true, badEnvelope: 3})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatalf("OpenState: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
			log, state := watch.NewLog(conn, time.Now, nil), watch.NewState(conn)
			engine := watch.NewEngine(conn, log, state, watch.EngineOptions{Batch: 1})
			src := emptyBaseline{sync: func(_ context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
				return sync(t, s, req)
			}}
			for range 2 { // the baseline, then the new mail
				if _, err := engine.Pass(t.Context(), "host", "INBOX", src, watch.PassOptions{Mode: watch.ModePoll}); err != nil {
					t.Fatalf("Pass: %v", err)
				}
			}
			page, err := log.Read(t.Context(), watch.Query{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range page.Events {
				if e.Kind == watch.KindNew {
					got = append(got, e.Ref)
				}
			}
			var want []string
			for _, uid := range []uint32{1, 2, 4} {
				want = append(want, ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: scriptUIDValidity, UID: uid}.String())
			}
			if !slices.Equal(got, want) {
				t.Errorf("new events = %q, want %q", got, want)
			}
			row, ok, err := state.Folder(t.Context(), "host", "INBOX")
			if err != nil || !ok || row.UIDNext != 5 {
				t.Errorf("row = %+v, %v, %v; want UIDNEXT 5", row, ok, err)
			}
		})
	}
}
