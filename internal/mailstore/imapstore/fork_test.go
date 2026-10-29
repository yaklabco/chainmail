package imapstore

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestForkExtensionsPresent fails to build against upstream go-imap, which
// has none of these. It proves the replace directive points at the fork.
func TestForkExtensionsPresent(t *testing.T) {
	t.Parallel()
	criteria := imap.SearchCriteria{GmailRaw: []string{"in:inbox"}, GmailMsgID: []uint64{1}, GmailThreadID: []uint64{2}}
	options := imap.FetchOptions{GmailMsgID: true, GmailThreadID: true, GmailLabels: true, Preview: true}
	if len(criteria.GmailRaw) != 1 || !options.Preview {
		t.Fatal("fork fields did not hold their values")
	}
	if imap.CapGmailExt1 != "X-GM-EXT-1" {
		t.Errorf("CapGmailExt1 = %q", imap.CapGmailExt1)
	}
}
