package setup

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
)

// Backup is the vault key backup choice, set by --key-backup or the prompt.
type Backup string

// The backup choices.
const (
	// BackupNone keeps the key only on this machine. It is the default.
	BackupNone Backup = "none"
	// BackupShow prints the key once on the terminal.
	BackupShow Backup = "show"
)

// backupHint names the valid --key-backup values.
const backupHint = "pass --key-backup none or --key-backup show"

// backupTries is how many answers AskBackup reads before it gives up.
const backupTries = 3

// BackupPrompt is the exact prompt text from the spec's Part 3, ending in
// "Choose [1]: ".
const BackupPrompt = `Vault key backup

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

Choose [1]: `

// retryPrompt asks again after an answer that is not 1 or 2.
const retryPrompt = "Please enter 1 or 2.\nChoose [1]: "

// ParseBackup parses --key-backup. "" is BackupNone.
func ParseBackup(s string) (Backup, error) {
	switch Backup(s) {
	case "", BackupNone:
		return BackupNone, nil
	case BackupShow:
		return BackupShow, nil
	default:
		return "", refuse(backupHint, "unknown --key-backup value %q; want none or show", s)
	}
}

// AskBackup shows BackupPrompt on w and reads a line from r. "" and "1"
// are none, "2" is show; anything else asks again, at most three times,
// then is a *Refusal. The end of input counts as the default, none.
func AskBackup(r io.Reader, w io.Writer) (Backup, error) {
	prompt := BackupPrompt
	for range backupTries {
		if _, err := io.WriteString(w, prompt); err != nil {
			return "", fmt.Errorf("key backup prompt: %w", err)
		}
		line, err := readAnswer(r)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("key backup prompt: %w", err)
		}
		switch strings.TrimSpace(line) {
		case "", "1":
			return BackupNone, nil
		case "2":
			return BackupShow, nil
		}
		if err != nil {
			// The input ended on an answer that is not 1 or 2: nothing more
			// will come, so asking again cannot help.
			break
		}
		prompt = retryPrompt
	}
	return "", refuse(backupHint, "no valid key backup choice; want 1 or 2")
}

// GenerateKey returns a new X25519 identity in age-keygen's file format:
// "# created: <RFC 3339 time>\n# public key: <recipient>\n<AGE-SECRET-KEY-...>\n".
func GenerateKey(now time.Time) ([]byte, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate vault key: %w", err)
	}
	key := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		now.UTC().Format(time.RFC3339), id.Recipient(), id)
	return []byte(key), nil
}

// The lines that frame the key when ShowKey prints it.
const (
	showKeyHeader = "Vault key\n\n" +
		"Anyone with this key and a copy of the vault can read your mail passwords.\n" +
		"Anything that can see this terminal sees it now. Save it in a password manager.\n\n"
	showKeyFooter = "\nPress Enter once you have saved it, then clear your scrollback.\n"
)

// ShowKey writes the short warning, the key and a closing line to tty,
// then waits for a line from tty before returning. The end of input ends
// the wait.
func ShowKey(tty io.ReadWriter, key []byte) error {
	var b strings.Builder
	b.WriteString(showKeyHeader)
	b.Write(key)
	if len(key) > 0 && key[len(key)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(showKeyFooter)
	// The error never carries the key: the write error names only the step.
	if _, err := io.WriteString(tty, b.String()); err != nil {
		return fmt.Errorf("show vault key: %w", err)
	}
	if _, err := readAnswer(tty); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("show vault key: wait for Enter: %w", err)
	}
	return nil
}

// readAnswer reads one line from r, one byte at a time so nothing after the
// newline is consumed: later prompts read from the same terminal or pipe.
// The returned line has no newline. At the end of input it returns what was
// read with io.EOF.
func readAnswer(r io.Reader) (string, error) {
	var line []byte
	var buf [1]byte
	for {
		n, err := r.Read(buf[:])
		if n == 1 {
			if buf[0] == '\n' {
				return string(line), nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return string(line), err
		}
	}
}
