package imapstore

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// folderRefs returns the refs of every message in folder, in UID order.
func folderRefs(t *testing.T, s *Store, folder string) []ref.Ref {
	t.Helper()
	res, err := s.Search(t.Context(), mailstore.SearchRequest{In: folder})
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("Search(in:%s) = %+v, %v", folder, res, err)
	}
	var out []ref.Ref
	for _, uid := range res[0].UIDs {
		out = append(out, ref.Ref{Account: s.acct.Name, Folder: folder, UIDValidity: res[0].Source.UIDValidity, UID: uid})
	}
	slices.SortFunc(out, func(a, b ref.Ref) int { return cmp.Compare(a.UID, b.UID) })
	return out
}

func TestFindThreadStandard(t *testing.T) {
	t.Parallel()
	const (
		root    = "From: ann@example.com\r\nSubject: root\r\nMessage-ID: <root@example.com>\r\n\r\nbody\r\n"
		other   = "From: ann@example.com\r\nSubject: other\r\nMessage-ID: <other@example.com>\r\n\r\nbody\r\n"
		replied = "From: me@example.test\r\nSubject: Re: root\r\nMessage-ID: <reply@example.test>\r\n" +
			"In-Reply-To: <root@example.com>\r\nReferences: <root@example.com>\r\n\r\nbody\r\n"
		// archived names the root in In-Reply-To only, as a client that drops
		// References writes it. The server search still finds it.
		archived = "From: bob@example.com\r\nSubject: Re: root\r\nMessage-ID: <late@example.com>\r\n" +
			"In-Reply-To: <root@example.com>\r\n\r\nbody\r\n"
		binned = "From: bob@example.com\r\nSubject: Re: root\r\nMessage-ID: <binned@example.com>\r\n" +
			"References: <root@example.com>\r\n\r\nbody\r\n"
	)
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Folders: []string{"Archive", "Sent", "Trash"},
		Mail: map[string][]string{"INBOX": {root, other}, "Sent": {replied}, "Archive": {archived}, "Trash": {binned}},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	// Archive sorts before INBOX and Sent: refs come by folder, then UID.
	thread := []ref.Ref{folderRefs(t, s, "Archive")[0], folderRefs(t, s, "INBOX")[0], folderRefs(t, s, "Sent")[0]}
	tests := []struct {
		name      string
		threadKey string
		root      string
		want      []ref.Ref
	}{
		{"by its root", "", "root@example.com", thread},
		{"a root in angle brackets", "", "<root@example.com>", thread},
		{"the key is ignored off Google", "ffff", "root@example.com", thread},
		{"an unknown root", mailstore.ThreadKey("missing@example.com"), "missing@example.com", []ref.Ref{}},
		{"no root", mailstore.ThreadKey("root@example.com"), "", []ref.Ref{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.FindThread(t.Context(), tc.threadKey, tc.root)
			if err != nil {
				t.Fatalf("FindThread(%q, %q): %v", tc.threadKey, tc.root, err)
			}
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("FindThread(%q, %q) = %v, want %v", tc.threadKey, tc.root, got, tc.want)
			}
		})
	}
}

func TestGmailFindThread(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	key := func(i int) string { return strconv.FormatUint(f.srv.ThreadID(i), 16) }
	if strings.ToUpper(key(0)) == key(0) {
		t.Fatalf("thread key %q has no hex letters, so the uppercase case would test nothing", key(0))
	}
	tests := []struct {
		name      string
		threadKey string
		root      string
		want      []uint64 // X-GM-MSGIDs, in UID order
	}{
		{"the invoice and its reply", key(0), "", []uint64{f.srv.MsgID(0), f.srv.MsgID(1)}},
		{"the root is ignored on Google", key(0), "missing@example.com", []uint64{f.srv.MsgID(0), f.srv.MsgID(1)}},
		{"a thread only in Trash", key(2), "", nil},
		{"an unknown thread", "ffffffffff", "inv@example.com", nil},
		{"not hex", "zz", "inv@example.com", nil},
		{"a leading zero", "0" + key(0), "", nil},
		{"uppercase hex", strings.ToUpper(key(0)), "", nil},
		{"thread 0", "0", "", nil},
		{"no key", "", "inv@example.com", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := f.store.FindThread(t.Context(), tc.threadKey, tc.root)
			if err != nil {
				t.Fatalf("FindThread(%q): %v", tc.threadKey, err)
			}
			if got == nil {
				t.Fatalf("FindThread(%q) = nil, want a non-nil slice", tc.threadKey)
			}
			var ids []uint64
			for _, r := range got {
				if r.Account != "gmail" || r.Folder != "[Gmail]/All Mail" || r.UIDValidity == 0 || r.UID == 0 {
					t.Errorf("FindThread(%q) gave %v, want All Mail refs", tc.threadKey, r)
				}
				ids = append(ids, r.GmailMsgID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("FindThread(%q) X-GM-MSGIDs = %v, want %v", tc.threadKey, ids, tc.want)
			}
			if !slices.IsSortedFunc(got, func(a, b ref.Ref) int { return cmp.Compare(a.UID, b.UID) }) {
				t.Errorf("FindThread(%q) = %v, want UID order", tc.threadKey, got)
			}
		})
	}
}
