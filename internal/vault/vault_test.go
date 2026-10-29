package vault

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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

func TestCheckKeyFile(t *testing.T) {
	t.Parallel()
	const me, other, wheel = 1000, 1001, 10
	tests := []struct {
		name     string
		mode     fs.FileMode
		uid, gid uint32
		euid     uint32
		wantErr  string
	}{
		{name: "0600 own", mode: 0o600, uid: me, gid: me, euid: me},
		{name: "0400 own", mode: 0o400, uid: me, gid: me, euid: me},
		{name: "0640 own", mode: 0o640, uid: me, gid: me, euid: me, wantErr: "too open"},
		{name: "0604 own", mode: 0o604, uid: me, gid: me, euid: me, wantErr: "too open"},
		{name: "0440 root:root (systemd ACL)", mode: 0o440, uid: 0, gid: 0, euid: me},
		{name: "0400 root:root", mode: 0o400, uid: 0, gid: 0, euid: me},
		{name: "0440 root:wheel", mode: 0o440, uid: 0, gid: wheel, euid: me, wantErr: "group"},
		{name: "0444 root:root", mode: 0o444, uid: 0, gid: 0, euid: me, wantErr: "too open"},
		{name: "0460 root:root", mode: 0o460, uid: 0, gid: 0, euid: me, wantErr: "too open"},
		{name: "0600 other uid", mode: 0o600, uid: other, gid: other, euid: me, wantErr: "owned by root or the daemon user"},
		{name: "0400 root:root as root", mode: 0o400, uid: 0, gid: 0, euid: 0},
		{name: "0440 root:root as root", mode: 0o440, uid: 0, gid: 0, euid: 0},
	}
	for _, tt := range tests {
		err := checkKeyFile(tt.mode, tt.uid, tt.gid, tt.euid)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != "" && err == nil:
			t.Errorf("%s: accepted, want error containing %q", tt.name, tt.wantErr)
		case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
			t.Errorf("%s: err = %v, want it to contain %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestLoadIdentityRejectsTwoIdentities(t *testing.T) {
	t.Parallel()
	a, b := newIdentity(t), newIdentity(t)
	path := filepath.Join(t.TempDir(), "two")
	if err := os.WriteFile(path, []byte(a.String()+"\n"+b.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIdentity(path)
	if err == nil || !strings.Contains(err.Error(), "exactly one identity") {
		t.Errorf("err = %v, want exactly one identity", err)
	}
	if err != nil && (strings.Contains(err.Error(), a.String()) || strings.Contains(err.Error(), b.String())) {
		t.Error("error message contains key material")
	}
}

// writeRawVault encrypts plain to id and writes it to path, bypassing Set.
func writeRawVault(t *testing.T, path string, id *age.X25519Identity, plain string) {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenValidatesLoadedSecrets(t *testing.T) {
	t.Parallel()
	// "aHVudGVyMg==" is base64 for "hunter2".
	tests := []struct {
		name    string
		plain   string
		wantErr error
		wantKey string
	}{
		{name: "invalid name", plain: `{"version":1,"secrets":{"accounts/ok":"aHVudGVyMg==","../Bad":"aHVudGVyMg=="}}`, wantErr: ErrInvalidName, wantKey: "../Bad"},
		{name: "empty value", plain: `{"version":1,"secrets":{"accounts/empty":""}}`, wantErr: ErrEmptyValue, wantKey: "accounts/empty"},
	}
	for _, tt := range tests {
		id := newIdentity(t)
		path := filepath.Join(t.TempDir(), "vault.age")
		writeRawVault(t, path, id, tt.plain)
		_, err := Open(path, id)
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantKey) {
			t.Errorf("%s: err = %v, want it to name %q", tt.name, err, tt.wantKey)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "aHVudGVyMg") {
			t.Errorf("%s: error contains a secret value: %v", tt.name, err)
		}
	}
}
