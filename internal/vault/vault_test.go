package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
)

func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func openTemp(t *testing.T) (*Vault, string, *age.X25519Identity) {
	t.Helper()
	id := newIdentity(t)
	path := filepath.Join(t.TempDir(), "vault.age")
	v, err := Open(path, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return v, path, id
}

func TestSetGet(t *testing.T) {
	t.Parallel()
	v, _, _ := openTemp(t)
	if err := v.Set("accounts/work", []byte("hunter2")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := v.Get("accounts/work")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hunter2" {
		t.Errorf("Get = %q", got)
	}
}

func TestPersistsAcrossOpen(t *testing.T) {
	t.Parallel()
	v, path, id := openTemp(t)
	if err := v.Set("tokens/gmail", []byte("rt")); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, id)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := again.Get("tokens/gmail")
	if err != nil || string(got) != "rt" {
		t.Errorf("after reopen Get = %q, %v", got, err)
	}
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	v, _, _ := openTemp(t)
	if names := v.Names(); len(names) != 0 {
		t.Errorf("Names() = %v, want empty", names)
	}
}

func TestOpenWithWrongIdentityFails(t *testing.T) {
	t.Parallel()
	v, path, _ := openTemp(t)
	if err := v.Set("accounts/a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, newIdentity(t)); err == nil {
		t.Error("Open with the wrong identity succeeded")
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	v, _, _ := openTemp(t)
	if _, err := v.Get("accounts/none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "accounts/work", valid: true},
		{name: "oauth_clients/workspace", valid: true},
		{name: "tokens/gmail-2", valid: true},
		{name: "", valid: false},
		{name: "Accounts/x", valid: false},
		{name: "/x", valid: false},
		{name: "x/", valid: false},
		{name: "a//b", valid: false},
		{name: "../x", valid: false},
		{name: "a b", valid: false},
		{name: strings.Repeat("a", 129), valid: false},
	}
	v, _, _ := openTemp(t)
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.valid {
			t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.valid)
		}
		err := v.Set(tt.name, []byte("x"))
		if tt.valid && err != nil {
			t.Errorf("Set(%q): %v", tt.name, err)
		}
		if !tt.valid && !errors.Is(err, ErrInvalidName) {
			t.Errorf("Set(%q) err = %v, want ErrInvalidName", tt.name, err)
		}
	}
	want := []string{"accounts/work", "oauth_clients/workspace", "tokens/gmail-2"}
	if got := v.Names(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestSetRejectsEmptyValue(t *testing.T) {
	t.Parallel()
	v, _, _ := openTemp(t)
	if err := v.Set("accounts/a", nil); !errors.Is(err, ErrEmptyValue) {
		t.Errorf("err = %v, want ErrEmptyValue", err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	t.Parallel()
	v, _, _ := openTemp(t)
	if err := v.Set("accounts/a", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	got, _ := v.Get("accounts/a")
	got[0] = 'X'
	again, _ := v.Get("accounts/a")
	if string(again) != "abc" {
		t.Errorf("stored value changed to %q", again)
	}
}

func TestFileIsPrivateAndEncrypted(t *testing.T) {
	t.Parallel()
	v, path, _ := openTemp(t)
	if err := v.Set("accounts/a", []byte("hunter2")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("hunter2")) || bytes.Contains(data, []byte("accounts/a")) {
		t.Error("vault file contains plaintext")
	}
	if !bytes.HasPrefix(data, []byte("age-encryption.org/v1")) {
		t.Error("vault file is not an age file")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".vault-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestLoadIdentity(t *testing.T) {
	t.Parallel()
	id := newIdentity(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("# created: now\n"+id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadIdentity(good)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if got.String() != id.String() {
		t.Error("loaded a different identity")
	}

	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte(id.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(open); err == nil || !strings.Contains(err.Error(), "too open") {
		t.Errorf("0644 key err = %v, want too open", err)
	}

	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(garbage); err == nil {
		t.Error("garbage key loaded")
	}
}

func TestConcurrentSet(t *testing.T) {
	t.Parallel()
	v, _, _ := openTemp(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if err := v.Set(fmt.Sprintf("accounts/a%d", i), []byte("x")); err != nil {
				t.Errorf("Set: %v", err)
			}
		})
	}
	wg.Wait()
	if n := len(v.Names()); n != 20 {
		t.Errorf("Names() has %d entries, want 20", n)
	}
}
