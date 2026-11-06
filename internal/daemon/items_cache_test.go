package daemon_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// itemCacheConfig watches INBOX and Archive with the cache on and no
// pause between backfill batches.
const itemCacheConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
watch = ["INBOX", "Archive"]

[cache]
backfill_pause = "0s"
`

// TestLinkedThreadsFromTheCacheAndItemSearch links a cached thread,
// archives its message, and checks that the view follows it from the
// cache, that item:<id> finds it and item:none finds only the other mail,
// both answered from the cache, and that the search audit rows record
// item_op=1 and never the ID.
func TestLinkedThreadsFromTheCacheAndItemSearch(t *testing.T) {
	t.Parallel()
	host := fake.New("host", false)
	day := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	root := host.Add(fake.Message{Folder: "INBOX", Date: day, From: itemAnn, Subject: "plan", MessageID: "root@example.com", Body: "kickoff"})
	lunch := host.Add(fake.Message{Folder: "INBOX", Date: day, From: itemAnn, Subject: "lunch", MessageID: "lunch@example.com", Body: "noon"})
	h := daemontest.Start(t, itemCacheConfig, daemontest.Options{
		Watch: true, Cache: true,
		// The archive expunges INBOX's copy; without a short floor the
		// sweep that drops its cached row waits 5 minutes.
		WatchSweepFloor: 10 * time.Millisecond,
		Secrets:         map[string]string{"accounts/host": "hunter2"},
		NewStore:        func(config.ResolvedAccount) mailstore.Store { return host },
	})
	awaitReadCacheReady(t, h)
	id := itemTask(t, h, "Ship the plan")
	link := itemLink(t, h, &chainmailv1.LinkThreadRequest{ItemId: id, Ref: root.String(), Anchor: true}).GetLink()
	if link.GetState() != "ok" || link.GetAnchorRef() != root.String() || link.GetMessages() != 1 {
		t.Fatalf("link = %v", link)
	}
	archived, err := h.Client.Organize.Archive(t.Context(), connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: []string{root.String()}}))
	if err != nil {
		t.Fatal(err)
	}
	moved := archived.Msg.GetOutcomes()[0].GetNewRef()
	if moved == "" {
		t.Fatalf("archive = %v, want a new ref in Archive", archived.Msg)
	}
	deadline := time.Now().Add(cacheReadWait)
	for {
		th := itemGet(t, h, id).GetThreads()[0]
		if th.GetSource() == "cache" && th.GetState() == "ok" && th.GetMessages() == 1 && th.GetLatest().GetRef() == moved && th.GetAnchorRef() == moved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %v the link is still %v, want it in Archive from the cache", cacheReadWait, th)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, tt := range []struct{ query, want string }{{query: "item:" + id, want: moved}, {query: "item:none", want: lunch.String()}} {
		var got []string
		for deadline := time.Now().Add(cacheReadWait); ; {
			resp, err := h.Client.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{QueryString: tt.query}))
			if err == nil {
				got = got[:0]
				for _, r := range resp.Msg.GetRows() {
					got = append(got, r.GetRef())
				}
				if slices.Equal(got, []string{tt.want}) && resp.Msg.GetSource() == "cache" {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: rows %v (last error %v), want [%s] from the cache", tt.query, got, err, tt.want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	marked := false
	for _, r := range h.AuditRows(t) {
		if r.Action != "search" {
			continue
		}
		if strings.Contains(r.Targets, id) {
			t.Errorf("search audit %+v records the item ID", r)
		}
		marked = marked || strings.Contains(r.Targets, `"item_op=1"`)
	}
	if !marked {
		t.Error("no search audit row records item_op=1")
	}
}
