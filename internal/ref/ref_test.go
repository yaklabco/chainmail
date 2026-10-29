package ref

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRefRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  Ref
		want string
	}{
		{Ref{Account: "yaklab", Folder: "INBOX", UIDValidity: 1700000000, UID: 42}, "cm1:yaklab:INBOX:1700000000:42"},
		{
			Ref{Account: "gmail", Folder: "[Gmail]/All Mail", UIDValidity: 11, UID: 9, GmailMsgID: 1278455344230334865},
			"cm1:gmail:%5BGmail%5D%2FAll%20Mail:11:9:g1278455344230334865",
		},
		{Ref{Account: "a", Folder: "Café:Notes", UIDValidity: 1, UID: 1}, "cm1:a:Caf%C3%A9%3ANotes:1:1"},
		{Ref{Account: "a", Folder: "a-b_c.d~e", UIDValidity: 1, UID: 1}, "cm1:a:a-b_c.d~e:1:1"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := tt.ref.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			back, err := Parse(tt.want)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if back != tt.ref {
				t.Errorf("Parse = %+v, want %+v", back, tt.ref)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"", "cm1", "cm2:a:INBOX:1:1", "cm1:a:INBOX:1", "cm1:a:INBOX:1:1:g1:extra",
		"cm1:A:INBOX:1:1", "cm1:a::1:1", "cm1:a:INBOX:0:1", "cm1:a:INBOX:1:0", "cm1:a:INBOX:x:1",
		"cm1:a:INBOX:01:1", "cm1:a:INBOX:1:4294967296", "cm1:a:INBOX:1:1:x5", "cm1:a:INBOX:1:1:g0",
		"cm1:a:IN BOX:1:1", "cm1:a:%5bGmail%5d:1:1", "cm1:a:%ZZ:1:1", "cm1:a:%FF:1:1",
		"cm1:a:IN%00BOX:1:1", "cm1:a:INBOX%0D:1:1", "cm1:a:INBOX%0A:1:1", "cm1:a:IN%1FBOX:1:1", "cm1:a:IN%7FBOX:1:1",
		"cm1:a:" + strings.Repeat("x", 3000) + ":1:1",
	} {
		if _, err := Parse(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", in, err)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	c := Cursor{
		Query: Fingerprint("from:alex", "", "gmail,yaklab"),
		Sources: []CursorSource{
			{Account: "gmail", Folder: "[Gmail]/All Mail", UIDValidity: 11, Below: 900},
			{Account: "yaklab", Folder: "INBOX", UIDValidity: 1700000000, Done: true},
		},
	}
	s := EncodeCursor(c)
	if !strings.HasPrefix(s, "c1.") || strings.ContainsAny(s, "+/=") {
		t.Fatalf("EncodeCursor = %q, want c1. and base64url", s)
	}
	back, err := DecodeCursor(s)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !reflect.DeepEqual(back, c) {
		t.Errorf("DecodeCursor = %+v, want %+v", back, c)
	}
}

func TestDecodeCursorRejects(t *testing.T) {
	t.Parallel()
	good := EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "a", Folder: "INBOX", UIDValidity: 1}}})
	for _, in := range []string{
		"", "c1.", "c2." + strings.TrimPrefix(good, "c1."), good + "!", "c1.bm90IGpzb24",
		EncodeCursor(Cursor{Query: Fingerprint("x")}),
		EncodeCursor(Cursor{Query: "short", Sources: []CursorSource{{Account: "a", Folder: "INBOX", UIDValidity: 1}}}),
		EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "Bad", Folder: "INBOX", UIDValidity: 1}}}),
		EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "a", Folder: "", UIDValidity: 1}}}),
		EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "a", Folder: "INBOX"}}}),
		EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "a", Folder: "IN\x00BOX", UIDValidity: 1}}}),
		EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "a", Folder: "INBOX\r\n", UIDValidity: 1}}}),
		EncodeCursor(Cursor{Query: Fingerprint("x"), Sources: []CursorSource{{Account: "a", Folder: "IN\x7fBOX", UIDValidity: 1}}}),
		"c1." + strings.Repeat("A", 70000),
	} {
		if _, err := DecodeCursor(in); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%.40q) = %v, want ErrInvalidCursor", in, err)
		}
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	a := Fingerprint("from:alex", "inbox")
	if len(a) != 16 {
		t.Errorf("Fingerprint length = %d, want 16 hex digits", len(a))
	}
	if a != Fingerprint("from:alex", "inbox") {
		t.Error("Fingerprint is not stable")
	}
	if a == Fingerprint("from:alexinbox") || a == Fingerprint("inbox", "from:alex") {
		t.Error("Fingerprint does not separate its parts")
	}
}
