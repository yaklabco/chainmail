//go:build live

package live

import (
	"os"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
)

// These tests read real mail, so they log counts and refs only, never
// subjects, addresses or bodies. They only read: nothing here sets or clears
// a flag.

// maxSearchPages bounds how far searchRows pages looking for a row.
const maxSearchPages = 20

func liveClient(t *testing.T) *client.Client {
	t.Helper()
	socket := os.Getenv("CHAINMAIL_SOCKET")
	if socket == "" {
		t.Fatal("set CHAINMAIL_SOCKET to the running daemon's socket")
	}
	c := client.New(socket, "live-test")
	t.Cleanup(c.Close)
	return c
}

func liveAccounts(t *testing.T, c *client.Client) []*chainmailv1.Account {
	t.Helper()
	list, err := c.Account.ListAccounts(t.Context(), connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		t.Fatalf("ListAccounts: %v (%s)", err, hint(err))
	}
	if len(list.Msg.GetAccounts()) == 0 {
		t.Fatal("no accounts configured")
	}
	return list.Msg.GetAccounts()
}

// reportPartial fails the test for every source a call could not read. The
// rest of the response is still valid, so the test carries on.
func reportPartial(t *testing.T, call string, partial []*chainmailv1.PartialSource) {
	t.Helper()
	for _, p := range partial {
		t.Errorf("%s: could not read %s folder %q: %s: %s (%s)", call, p.GetAccount(), p.GetFolder(), p.GetReason(), p.GetMessage(), p.GetHint())
	}
}

// searchRows searches one account and returns the first page that has rows,
// or no rows when the search ends without any. A page can be short, even
// empty, while next_cursor is set, so it follows the cursor.
func searchRows(t *testing.T, c *client.Client, account, query string, limit uint32) []*chainmailv1.MessageRow {
	t.Helper()
	cursor := ""
	for page := 1; page <= maxSearchPages; page++ {
		start := time.Now()
		resp, err := c.Message.Search(t.Context(), connect.NewRequest(&chainmailv1.SearchRequest{
			QueryString: query, Accounts: []string{account}, Limit: limit, Cursor: cursor,
		}))
		if err != nil {
			t.Fatalf("Search page %d: %v (%s)", page, err, hint(err))
		}
		reportPartial(t, "Search", resp.Msg.GetPartial())
		rows := resp.Msg.GetRows()
		t.Logf("search page %d: %d rows, %d matches from here on, in %v", page, len(rows), resp.Msg.GetCount(), time.Since(start))
		cursor = resp.Msg.GetNextCursor()
		if len(rows) > 0 || cursor == "" {
			return rows
		}
	}
	t.Fatalf("Search: no rows in %d pages, and next_cursor is still set", maxSearchPages)
	return nil
}

// getOne reads one message and fails the test if the call or its item failed.
func getOne(t *testing.T, c *client.Client, ref string, full bool) *chainmailv1.Message {
	t.Helper()
	resp, err := c.Message.Get(t.Context(), connect.NewRequest(&chainmailv1.GetRequest{Refs: []string{ref}, Full: full}))
	if err != nil {
		t.Fatalf("Get %s: %v (%s)", ref, err, hint(err))
	}
	items := resp.Msg.GetItems()
	if len(items) != 1 {
		t.Fatalf("Get %s: %d items, want 1", ref, len(items))
	}
	if e := items[0].GetError(); e != nil {
		t.Fatalf("Get %s: %s: %s (%s)", ref, e.GetReason(), e.GetMessage(), e.GetHint())
	}
	m := items[0].GetMessage()
	if m == nil {
		t.Fatalf("Get %s: the item has neither a message nor an error", ref)
	}
	return m
}

func TestEveryAccountConnects(t *testing.T) {
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			resp, err := c.Account.TestAccount(t.Context(), connect.NewRequest(&chainmailv1.TestAccountRequest{Account: a.GetName()}))
			if err != nil {
				t.Fatalf("%v (%s)", err, hint(err))
			}
			for _, check := range []struct {
				name string
				c    *chainmailv1.Check
			}{{"imap", resp.Msg.GetImap()}, {"smtp", resp.Msg.GetSmtp()}} {
				if !check.c.GetOk() {
					t.Errorf("%s: %s: %s", check.name, check.c.GetReason(), check.c.GetError())
					continue
				}
				t.Logf("%s ok in %dms, capabilities %v", check.name, check.c.GetLatencyMs(), check.c.GetCapabilities())
			}
		})
	}
}

// TestEveryAccountReads lists each account's folders, searches it, and reads
// the newest message and its thread.
func TestEveryAccountReads(t *testing.T) {
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			folders, err := c.Message.ListFolders(t.Context(), connect.NewRequest(&chainmailv1.ListFoldersRequest{Accounts: []string{a.GetName()}}))
			if err != nil {
				t.Fatalf("ListFolders: %v (%s)", err, hint(err))
			}
			reportPartial(t, "ListFolders", folders.Msg.GetPartial())
			if n := len(folders.Msg.GetAccounts()); n != 1 {
				t.Fatalf("ListFolders: %d accounts in the response, want 1", n)
			}
			var roles []string
			for _, f := range folders.Msg.GetAccounts()[0].GetFolders() {
				roles = append(roles, f.GetRole())
			}
			if !slices.Contains(roles, "inbox") {
				t.Errorf("no inbox among %d folders", len(roles))
			}
			t.Logf("%d folders", len(roles))

			rows := searchRows(t, c, a.GetName(), "", 5)
			if len(rows) == 0 {
				t.Fatal("the account has no mail to read")
			}
			r := rows[0].GetRef()
			m := getOne(t, c, r, false)
			body := m.GetBody()
			t.Logf("get %s: body %d bytes, truncated %v, quote trimmed %v (%d bytes), %d attachments",
				r, len(body.GetUntrusted()), body.GetTruncated(), body.GetQuotedTrimmed(), body.GetTrimmedBytes(), len(m.GetAttachments()))

			thread, err := c.Message.GetThread(t.Context(), connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: r}))
			if err != nil {
				t.Fatalf("GetThread %s: %v (%s)", r, err, hint(err))
			}
			if len(thread.Msg.GetMessages()) == 0 {
				t.Errorf("GetThread %s: no messages", r)
			}
			t.Logf("thread of %s: %d messages, truncated %v, %d omitted refs",
				r, len(thread.Msg.GetMessages()), thread.Msg.GetTruncated(), len(thread.Msg.GetOmittedRefs()))
		})
	}
}

// TestReadsLeaveUnreadMailUnread reads one unread message per account with
// get, thread, attachment (when it has one) and raw, and checks after each
// read that it is still unread. It prefers an unread message with an
// attachment, so the attachment read is covered when one exists.
func TestReadsLeaveUnreadMailUnread(t *testing.T) {
	c := liveClient(t)
	for _, a := range liveAccounts(t, c) {
		t.Run(a.GetName(), func(t *testing.T) {
			rows := searchRows(t, c, a.GetName(), "is:unread", 50)
			if len(rows) == 0 {
				t.Skip("no unread message to check; mark one unread in another client and run again")
			}
			seed := rows[0]
			if i := slices.IndexFunc(rows, (*chainmailv1.MessageRow).GetHasAttachment); i >= 0 {
				seed = rows[i]
			}
			r := seed.GetRef()
			if slices.Contains(seed.GetFlags(), `\Seen`) {
				t.Fatalf("is:unread returned %s, which is already \\Seen", r)
			}
			stillUnread := func(after string) {
				t.Helper()
				if slices.Contains(getOne(t, c, r, false).GetFlags(), `\Seen`) {
					t.Fatalf("%s is \\Seen after %s; mark it unread again in another client", r, after)
				}
			}

			atts := getOne(t, c, r, true).GetAttachments()
			stillUnread("get")

			if _, err := c.Message.GetThread(t.Context(), connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: r})); err != nil {
				t.Fatalf("GetThread %s: %v (%s)", r, err, hint(err))
			}
			stillUnread("thread")

			if len(atts) > 0 {
				part := atts[0].GetPart()
				n := downloadAttachment(t, c, r, part)
				t.Logf("downloaded %s part %s: %d bytes", r, part, n)
				stillUnread("attachment")
			} else {
				t.Logf("%s has no attachment; the attachment read was not checked", r)
			}

			n := readRaw(t, c, r)
			if n == 0 {
				t.Errorf("GetRaw %s: no bytes", r)
			}
			t.Logf("raw %s: %d bytes", r, n)
			stillUnread("raw")
		})
	}
}

// downloadAttachment streams one attachment and returns its decoded size.
func downloadAttachment(t *testing.T, c *client.Client, ref, part string) int {
	t.Helper()
	stream, err := c.Message.DownloadAttachment(t.Context(), connect.NewRequest(&chainmailv1.DownloadAttachmentRequest{Ref: ref, Part: part}))
	if err != nil {
		t.Fatalf("DownloadAttachment %s %s: %v (%s)", ref, part, err, hint(err))
	}
	defer func() { _ = stream.Close() }() // drained or failed by then; Err reports the outcome
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			t.Fatalf("DownloadAttachment %s %s: %v (%s)", ref, part, err, hint(err))
		}
		t.Fatalf("DownloadAttachment %s %s: the stream ended before the header", ref, part)
	}
	if stream.Msg().GetHeader() == nil {
		t.Fatalf("DownloadAttachment %s %s: the first message is not the header", ref, part)
	}
	var n int
	for stream.Receive() {
		n += len(stream.Msg().GetData())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("DownloadAttachment %s %s: %v (%s)", ref, part, err, hint(err))
	}
	return n
}

// readRaw streams a message's original bytes and returns how many there were.
func readRaw(t *testing.T, c *client.Client, ref string) int {
	t.Helper()
	stream, err := c.Message.GetRaw(t.Context(), connect.NewRequest(&chainmailv1.GetRawRequest{Ref: ref}))
	if err != nil {
		t.Fatalf("GetRaw %s: %v (%s)", ref, err, hint(err))
	}
	defer func() { _ = stream.Close() }() // as in downloadAttachment
	var n int
	for stream.Receive() {
		n += len(stream.Msg().GetData())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("GetRaw %s: %v (%s)", ref, err, hint(err))
	}
	return n
}
