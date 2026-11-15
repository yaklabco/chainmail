package setup

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/yaklabco/chainmail/internal/vault"
)

// specBackupPrompt is the prompt block from the spec's Part 3, copied
// verbatim.
const specBackupPrompt = `Vault key backup

Your mail passwords are stored encrypted, and only the vault key can
decrypt them. The key belongs to the chainmail user, so you and any
agent you run cannot read it. That separation is what stops an agent
from going around chainmail's send policy.

  1. No backup (recommended)
     The key exists only on this machine, readable only by chainmail.
     If it is lost (disk failure, reinstall, moving to a new machine),
     the vault cannot be opened. You run bootstrap again and re-enter
     each account's password and redo any OAuth logins. No mail is lost.

  2. Show the key once
     The key is printed here, once, for you to save in a password
     manager. With it you can restore the vault on a new machine.
     Anything that can see this terminal right now sees the key:
     scrollback, screen sharing, terminal logs, and any agent watching
     this session. Anyone holding the key and a copy of the vault can
     read your mail passwords. Clear your scrollback afterwards.

Choose [1]:`

func TestGenerateKey(t *testing.T) {
	// A non-UTC time checks that the created line is written in UTC.
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.FixedZone("EET", 2*60*60))
	key, err := GenerateKey(now)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !bytes.HasSuffix(key, []byte("\n")) {
		t.Fatal("key does not end in a newline")
	}
	lines := strings.Split(strings.TrimSuffix(string(key), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("key has %d lines, want 3", len(lines))
	}
	if lines[0] != "# created: 2026-11-15T10:00:00Z" {
		t.Errorf("line 1 = %q, want the created line in UTC", lines[0])
	}
	if !strings.HasPrefix(lines[1], "# public key: age1") {
		t.Errorf("line 2 = %q, want a public key line", lines[1])
	}
	// Never print the secret line itself in a failure message.
	if !strings.HasPrefix(lines[2], "AGE-SECRET-KEY-1") {
		t.Error("line 3 is not an AGE-SECRET-KEY-1 identity")
	}

	ids, err := age.ParseIdentities(bytes.NewReader(key))
	if err != nil {
		t.Fatalf("ParseIdentities: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("parsed %d identities, want 1", len(ids))
	}
	id, ok := ids[0].(*age.X25519Identity)
	if !ok {
		t.Fatalf("identity is %T, want *age.X25519Identity", ids[0])
	}
	if got, want := strings.TrimPrefix(lines[1], "# public key: "), id.Recipient().String(); got != want {
		t.Errorf("public key line = %q, identity recipient = %q", got, want)
	}

	other, err := GenerateKey(now)
	if err != nil {
		t.Fatalf("second GenerateKey: %v", err)
	}
	if bytes.Equal(key, other) {
		t.Error("two calls returned the same key")
	}

	path := filepath.Join(t.TempDir(), "vault-key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := vault.LoadIdentity(path)
	if err != nil {
		t.Fatalf("vault.LoadIdentity: %v", err)
	}
	if loaded.Recipient().String() != id.Recipient().String() {
		t.Error("vault.LoadIdentity read a different identity")
	}
}

func TestParseBackup(t *testing.T) {
	tests := []struct {
		in   string
		want Backup
	}{
		{"", BackupNone},
		{"none", BackupNone},
		{"show", BackupShow},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseBackup(tt.in)
			if err != nil {
				t.Fatalf("ParseBackup(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseBackup(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	_, err := ParseBackup("yes")
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("ParseBackup(yes) error = %v, want a *Refusal", err)
	}
	text := r.Msg + " " + r.Hint
	for _, want := range []string{"none", "show"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q, hint %q does not name %q", r.Msg, r.Hint, want)
		}
	}
}

func TestBackupPromptText(t *testing.T) {
	if BackupPrompt != specBackupPrompt+" " {
		t.Errorf("BackupPrompt differs from the spec text.\ngot:\n%s\nwant:\n%s", BackupPrompt, specBackupPrompt+" ")
	}
}

func TestAskBackup(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Backup
		prompts int
	}{
		{"enter", "\n", BackupNone, 1},
		{"one", "1\n", BackupNone, 1},
		{"two", "2\n", BackupShow, 1},
		{"spaces", "  2 \n", BackupShow, 1},
		{"crlf", "2\r\n", BackupShow, 1},
		{"retry", "x\n2\n", BackupShow, 2},
		{"eof", "", BackupNone, 1},
		{"last line without newline", "2", BackupShow, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := AskBackup(strings.NewReader(tt.in), &out)
			if err != nil {
				t.Fatalf("AskBackup: %v", err)
			}
			if got != tt.want {
				t.Errorf("AskBackup = %q, want %q", got, tt.want)
			}
			if !strings.HasPrefix(out.String(), BackupPrompt) {
				t.Errorf("output does not start with BackupPrompt:\n%s", out.String())
			}
			if n := strings.Count(out.String(), "Choose [1]: "); n != tt.prompts {
				t.Errorf("asked %d times, want %d", n, tt.prompts)
			}
		})
	}

	t.Run("three wrong answers", func(t *testing.T) {
		var out bytes.Buffer
		_, err := AskBackup(strings.NewReader("x\nx\nx\n"), &out)
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("AskBackup error = %v, want a *Refusal", err)
		}
		if n := strings.Count(out.String(), "Choose [1]: "); n != 3 {
			t.Errorf("asked %d times, want 3", n)
		}
	})

	t.Run("reads no further than the answer", func(t *testing.T) {
		r := strings.NewReader("2\nnext line\n")
		if _, err := AskBackup(r, &bytes.Buffer{}); err != nil {
			t.Fatalf("AskBackup: %v", err)
		}
		if r.Len() != len("next line\n") {
			t.Errorf("AskBackup left %d bytes unread, want %d", r.Len(), len("next line\n"))
		}
	})
}

// fakeTTY is a terminal: reads come from in, writes go to out.
type fakeTTY struct {
	in  *strings.Reader
	out bytes.Buffer
}

func (f *fakeTTY) Read(p []byte) (int, error)  { return f.in.Read(p) }
func (f *fakeTTY) Write(p []byte) (int, error) { return f.out.Write(p) }

func TestShowKey(t *testing.T) {
	key, err := GenerateKey(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	tty := &fakeTTY{in: strings.NewReader("\nnot read\n")}
	if err := ShowKey(tty, key); err != nil {
		t.Fatalf("ShowKey: %v", err)
	}
	out := tty.out.String()

	// Never print out or key in a failure message: they hold the secret.
	if n := strings.Count(out, string(key)); n != 1 {
		t.Fatalf("output holds the key %d times, want 1", n)
	}
	keyAt := strings.Index(out, string(key))
	const warning = "Anyone with this key and a copy of the vault can read your mail passwords."
	warnAt := strings.Index(out, warning)
	if warnAt < 0 || warnAt > keyAt {
		t.Error("the warning line does not come before the key")
	}
	const closing = "Press Enter once you have saved it, then clear your scrollback."
	closeAt := strings.Index(out, closing)
	if closeAt < keyAt+len(key) {
		t.Error("the closing line does not follow the key")
	}
	if tty.in.Len() != len("not read\n") {
		t.Errorf("ShowKey left %d bytes unread, want %d", tty.in.Len(), len("not read\n"))
	}
}

// The end of input is not Enter: ShowKey must not return as if the user had
// acknowledged the key, and its error must not carry the key.
func TestShowKeyEOF(t *testing.T) {
	key, err := GenerateKey(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"", "partial"} {
		tty := &fakeTTY{in: strings.NewReader(in)}
		err := ShowKey(tty, key)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ShowKey with input %q returned %v, want an error wrapping io.EOF", in, err)
		}
		if strings.Contains(err.Error(), string(key)) || strings.Contains(err.Error(), "AGE-SECRET-KEY") {
			t.Fatal("the error carries the key")
		}
	}
}
