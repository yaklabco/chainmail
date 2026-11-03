package imapstore

import (
	"crypto/tls"
	"errors"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// searchedFolders returns the folders Search ran in for in.
func searchedFolders(t *testing.T, s *Store, in string) ([]string, error) {
	t.Helper()
	res, err := s.Search(t.Context(), mailstore.SearchRequest{In: in, CountOnly: true})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range res {
		out = append(out, r.Source.Folder)
	}
	return out, nil
}

// scopeServer starts a test server whose folders take roles by name.
func scopeServer(t *testing.T) (provider.Endpoint, *tlstest.CA) {
	t.Helper()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Folders: []string{"Sent", "Archive", "Trash", "Junk", "Projects"},
	})
	return ep, ca
}

func TestSearchFoldersStandard(t *testing.T) {
	t.Parallel()
	ep, ca := scopeServer(t)
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	tests := []struct {
		in      string
		want    []string
		wantErr error
	}{
		{"", []string{"INBOX", "Archive", "Projects", "Sent"}, nil},
		{"anywhere", []string{"INBOX", "Archive", "Junk", "Projects", "Sent", "Trash"}, nil},
		{"inbox", []string{"INBOX"}, nil},
		{"trash", []string{"Trash"}, nil},
		{"junk", []string{"Junk"}, nil},
		{"spam", []string{"Junk"}, nil},
		{"Projects", []string{"Projects"}, nil},
		{"drafts", nil, mailstore.ErrNotFound},
		{"Nope", nil, mailstore.ErrNotFound},
	}
	for _, tt := range tests {
		got, err := s.SearchFolders(t.Context(), tt.in)
		searched, serr := searchedFolders(t, s, tt.in)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) || !errors.Is(serr, tt.wantErr) {
				t.Errorf("SearchFolders(%q) = %q, %v and Search = %v; want %v from both", tt.in, got, err, serr, tt.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("SearchFolders(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
		if serr != nil || !slices.Equal(got, searched) {
			t.Errorf("SearchFolders(%q) = %q, but Search ran in %q (%v)", tt.in, got, searched, serr)
		}
	}
}

func TestSearchFoldersGmail(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{"", []string{"[Gmail]/All Mail"}},
		{"inbox", []string{"[Gmail]/All Mail"}},
		{"Receipts", []string{"[Gmail]/All Mail"}},
		{"trash", []string{"[Gmail]/Trash"}},
		{"spam", []string{"[Gmail]/Spam"}},
		{"anywhere", []string{"[Gmail]/All Mail", "[Gmail]/Trash", "[Gmail]/Spam"}},
	} {
		got, err := f.store.SearchFolders(t.Context(), tt.in)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("SearchFolders(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
		if searched, err := searchedFolders(t, f.store, tt.in); err != nil || !slices.Equal(got, searched) {
			t.Errorf("SearchFolders(%q) = %q, but Search ran in %q (%v)", tt.in, got, searched, err)
		}
	}
}

// SearchFolders reads the folder list the store keeps, as Search does, so
// a folder made by another client shows only once ListFolders has listed
// again; the two never disagree.
func TestSearchFoldersUsesTheListSearchUses(t *testing.T) {
	t.Parallel()
	ep, ca := scopeServer(t)
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	before := []string{"INBOX", "Archive", "Projects", "Sent"}
	if got, err := s.SearchFolders(t.Context(), ""); err != nil || !slices.Equal(got, before) {
		t.Fatalf("SearchFolders = %q, %v; want %q", got, err, before)
	}
	c, err := imapclient.DialTLS(ep.Addr(), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: ca.Pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login("host@example.test", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("Later", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SearchFolders(t.Context(), ""); err != nil || !slices.Equal(got, before) {
		t.Errorf("SearchFolders before a new LIST = %q, %v; want the kept list %q", got, err, before)
	}
	if searched, err := searchedFolders(t, s, ""); err != nil || !slices.Equal(searched, before) {
		t.Errorf("Search before a new LIST ran in %q, %v; want %q", searched, err, before)
	}
	if _, err := s.ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := []string{"INBOX", "Archive", "Later", "Projects", "Sent"}
	if got, err := s.SearchFolders(t.Context(), ""); err != nil || !slices.Equal(got, after) {
		t.Errorf("SearchFolders after ListFolders = %q, %v; want %q", got, err, after)
	}
}
