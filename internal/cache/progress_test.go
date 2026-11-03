package cache

import (
	"reflect"
	"testing"
	"time"
)

func TestProgress(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if _, ok, err := s.Progress(t.Context(), testAccount, "INBOX"); err != nil || ok {
		t.Fatalf("Progress before any put: ok %v, err %v", ok, err)
	}
	fresh := Progress{Account: testAccount, Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 500}
	done := Progress{
		Account: testAccount, Folder: "Archive", UIDValidity: 9, BaselineUIDNext: 1200, BackfillLowUID: 1,
		BackfillDone: true, WindowDone: true, LastAppliedAt: t0.Add(-time.Minute),
	}
	other := Progress{Account: "other", Folder: "INBOX", UIDValidity: 3, BaselineUIDNext: 10}
	for _, p := range []Progress{fresh, done, other} {
		if err := s.PutProgress(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.Progress(t.Context(), testAccount, "INBOX")
	if err != nil || !ok || !reflect.DeepEqual(got, fresh) {
		t.Errorf("Progress = %+v, %v, %v; want %+v (zero LastAppliedAt stays zero)", got, ok, err, fresh)
	}
	all, err := s.AllProgress(t.Context(), testAccount)
	if err != nil || !reflect.DeepEqual(all, []Progress{done, fresh}) {
		t.Errorf("AllProgress = %+v, %v; want Archive then INBOX of acme only", all, err)
	}
	fresh.BackfillLowUID, fresh.LastAppliedAt = 250, t0
	if err := s.PutProgress(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.Progress(t.Context(), testAccount, "INBOX")
	if err != nil || !reflect.DeepEqual(got, fresh) {
		t.Errorf("Progress after update = %+v, %v; want %+v", got, err, fresh)
	}
}
