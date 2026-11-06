package daemon

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
)

// threadFinder returns h as a mailstore.ThreadFinder, failing the test when
// it is not one.
func threadFinder(t *testing.T, h mailstore.Store) mailstore.ThreadFinder {
	t.Helper()
	tf, ok := h.(mailstore.ThreadFinder)
	if !ok {
		t.Fatalf("%T does not implement mailstore.ThreadFinder", h)
	}
	return tf
}

func TestAccountStorePassesFindThreadThrough(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	day := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	root := fk.Add(fake.Message{Folder: "INBOX", Date: day, Subject: "root", MessageID: "root@example.net"})
	reply := fk.Add(fake.Message{Folder: "Sent", Date: day.Add(time.Hour), Subject: "reply", MessageID: "reply@example.net", References: []string{"<root@example.net>"}})
	s := &storeSet{build: func(config.ResolvedAccount) mailstore.Store { return fk }}
	tf := threadFinder(t, s.get(config.ResolvedAccount{Name: "host"}))
	got, err := tf.FindThread(t.Context(), mailstore.ThreadKey("root@example.net"), "root@example.net")
	if want := []ref.Ref{root, reply}; err != nil || !slices.Equal(got, want) {
		t.Errorf("FindThread = %v, %v; want %v", got, err, want)
	}
	if n := fk.Calls("FindThread"); n != 1 {
		t.Errorf("calls = %d, want the call passed through once", n)
	}
}

func TestAccountStoreWithoutFindThreadIsUnsupported(t *testing.T) {
	t.Parallel()
	s := &storeSet{build: func(acct config.ResolvedAccount) mailstore.Store { return storeOnly{fake.New(acct.Name, false)} }}
	tf := threadFinder(t, s.get(config.ResolvedAccount{Name: "host"}))
	if _, err := tf.FindThread(t.Context(), "", "root@example.net"); !errors.Is(err, mailstore.ErrUnsupported) {
		t.Errorf("FindThread on a store without it = %v, want ErrUnsupported", err)
	}
}

func TestAccountStoreFindThreadAfterClose(t *testing.T) {
	t.Parallel()
	s := &storeSet{build: func(acct config.ResolvedAccount) mailstore.Store { return fake.New(acct.Name, false) }}
	tf := threadFinder(t, s.get(config.ResolvedAccount{Name: "host"}))
	if err := s.closeAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.FindThread(t.Context(), "", "root@example.net"); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("FindThread after closeAll = %v, want ErrUnavailable", err)
	}
}
