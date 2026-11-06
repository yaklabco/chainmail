package mcpserver

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

// TestSearchRejectsUnknownSource checks a source other than auto, cache or
// server is refused before any daemon call.
func TestSearchRejectsUnknownSource(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"Cache", "local", "mixed"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			body := wantError(t, e.call(t, "search", map[string]any{"source": source}), api.ReasonInvalidArgument)
			if !strings.Contains(body.Message, "source must be auto, cache or server") {
				t.Errorf("message = %q", body.Message)
			}
			for _, c := range e.fake.Calls() {
				if c.Procedure == chainmailv1connect.MessageServiceSearchProcedure {
					t.Errorf("source %q reached the daemon", source)
				}
			}
		})
	}
}

// TestSearchReportsSources checks search returns what answered, overall and
// per account, with the cache's sync time and an incomplete reason.
func TestSearchReportsSources(t *testing.T) {
	t.Parallel()
	synced := time.Date(2026, 11, 2, 8, 58, 0, 0, time.UTC)
	f := &fakeDaemon{Search: func(context.Context, *chainmailv1.SearchRequest) (*chainmailv1.SearchResponse, error) {
		return &chainmailv1.SearchResponse{Count: 3, Source: "mixed", AccountCounts: []*chainmailv1.AccountCount{
			{Account: "gm", Count: 2, Source: "cache", SyncedAt: timestamppb.New(synced), Complete: true},
			{Account: "host", Count: 1, Source: "server", Complete: true},
			{Account: "work", Source: "cache", IncompleteReason: "backfilling"},
		}}, nil
	}}
	e := newTestEnv(t, f, latest)
	res := e.call(t, "search", map[string]any{"query": "from:ann", "count_only": true})
	wantOK(t, res)
	var out searchOut
	decode(t, res, &out)
	want := []accountCountOut{
		{Account: "gm", Count: 2, Source: "cache", SyncedAt: "2026-11-02T08:58:00Z", Complete: true},
		{Account: "host", Count: 1, Source: "server", Complete: true},
		{Account: "work", Source: "cache", IncompleteReason: "backfilling"},
	}
	if out.Source != "mixed" || !reflect.DeepEqual(out.AccountCounts, want) {
		t.Errorf("source %q, account_counts %+v; want mixed, %+v", out.Source, out.AccountCounts, want)
	}
}

// TestReadToolsReportSource checks get_message and get_thread say where
// each message, and the call as a whole, was read from.
func TestReadToolsReportSource(t *testing.T) {
	t.Parallel()
	cached := &chainmailv1.Message{Ref: testRef, Account: "work", Folder: "INBOX", Subject: readU("Hi"), Source: "cache"}
	fetched := &chainmailv1.Message{Ref: testRef2, Account: "work", Folder: "INBOX", Subject: readU("Re: Hi"), Source: "server"}
	f := &fakeDaemon{
		Get: func(context.Context, *chainmailv1.GetRequest) (*chainmailv1.GetResponse, error) {
			return &chainmailv1.GetResponse{Source: "mixed", Items: []*chainmailv1.GetItem{
				{Ref: testRef, Result: &chainmailv1.GetItem_Message{Message: cached}},
				{Ref: testRef2, Result: &chainmailv1.GetItem_Message{Message: fetched}},
			}}, nil
		},
		GetThread: func(context.Context, *chainmailv1.GetThreadRequest) (*chainmailv1.GetThreadResponse, error) {
			return &chainmailv1.GetThreadResponse{ThreadId: "t1", Source: "cache", Messages: []*chainmailv1.Message{cached}}, nil
		},
	}
	e := newTestEnv(t, f, latest)

	res := e.call(t, "get_message", map[string]any{"refs": []string{testRef, testRef2}})
	wantOK(t, res)
	var got getMessageOut
	decode(t, res, &got)
	if got.Source != "mixed" || len(got.Messages) != 2 || got.Messages[0].Source != "cache" || got.Messages[1].Source != "server" {
		t.Errorf("get_message source %q, messages %+v; want mixed, cache then server", got.Source, got.Messages)
	}

	res = e.call(t, "get_thread", map[string]any{"ref": testRef})
	wantOK(t, res)
	var thread getThreadOut
	decode(t, res, &thread)
	if thread.Source != "cache" || len(thread.Messages) != 1 || thread.Messages[0].Source != "cache" {
		t.Errorf("get_thread source %q, messages %+v; want cache throughout", thread.Source, thread.Messages)
	}
}

// TestSearchDescriptionNamesTheCache pins what the search description tells
// agents about the cache.
func TestSearchDescriptionNamesTheCache(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"local cache", `source: "server"`, `source: "cache"`, "incomplete_reason", "untrusted"} {
		if !strings.Contains(searchDescription, want) {
			t.Errorf("search description lacks %q: %q", want, searchDescription)
		}
	}
}

// TestSearchDescriptionNamesTheItemOperators: agents learn item:<id> and
// item:none from the search description (SP13 amendment A8).
func TestSearchDescriptionNamesTheItemOperators(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"item:<id>", "item:none", "work item"} {
		if !strings.Contains(searchDescription, want) {
			t.Errorf("search description lacks %q: %q", want, searchDescription)
		}
	}
}
