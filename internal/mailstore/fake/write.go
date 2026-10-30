package fake

// Writer methods of the fake store, with the semantics of spec 7.1 for
// standard and Gmail accounts. The fake is never more lenient than a
// server: unknown folders, stale refs, labels off Gmail and folder rules
// fail as imapstore reports them.
//
// On Gmail every message lives in All Mail, Trash or Spam, and its ref
// points there. A move or copy into a label folder (INBOX, Sent Mail,
// Drafts or a user label) edits labels and keeps the ref. Only moves into
// or out of Trash and Spam give a new ref.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Writer operation names, as Ops records them and FailWrite takes them.
const (
	opAppend          = "Append"
	opMove            = "Move"
	opCopy            = "Copy"
	opArchive         = "Archive"
	opTrash           = "Trash"
	opExpunge         = "Expunge"
	opStoreFlags      = "StoreFlags"
	opStoreLabels     = "StoreLabels"
	opCreateFolder    = "CreateFolder"
	opRenameFolder    = "RenameFolder"
	opDeleteFolder    = "DeleteFolder"
	opFindByMessageID = "FindByMessageID"
)

// delimiter separates folder levels, as on Gmail and the fake's folders.
const delimiter = "/"

// gmailPrefix starts the names of Gmail's own folders, which cannot be
// created, renamed or deleted.
const gmailPrefix = "[Gmail]"

// systemFlags are the IMAP flags a client may store. \Recent is the
// server's alone.
var systemFlags = []string{`\Seen`, `\Answered`, `\Flagged`, `\Deleted`, `\Draft`}

// systemLabels are the Gmail system labels StoreLabels accepts.
var systemLabels = []string{`\Inbox`, `\Starred`, `\Important`, `\Sent`, `\Draft`}

// FailWrite makes the next call of the Writer method op (such as "Move" or
// "Append") fail with err, then clears itself. A failed batch call fails
// every ref and changes nothing. A nil err clears a pending failure. Fail
// gives a failure that stays.
func (s *Store) FailWrite(op string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.writeFail, op)
		return
	}
	s.writeFail[op] = err
}

// Ops returns the name of every Writer call so far, failed ones included,
// in order.
func (s *Store) Ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ops)
}

// Appended returns the raw bytes of every successful Append, in order.
func (s *Store) Appended() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.appended))
	for i, b := range s.appended {
		out[i] = slices.Clone(b)
	}
	return out
}

// SetArchiveFolder makes Archive move to name on standard accounts, as the
// account's archive_folder setting does. "" goes back to the folder with
// the archive role. Gmail accounts archive by label and ignore it.
func (s *Store) SetArchiveFolder(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archiveFolder = name
}

// beginWrite records a Writer call and returns the error it must fail
// with: a pending FailWrite, a standing Fail, or a done context.
func (s *Store) beginWrite(ctx context.Context, op string) error {
	s.ops = append(s.ops, op)
	s.calls[op]++
	if err, ok := s.writeFail[op]; ok {
		delete(s.writeFail, op)
		return err
	}
	if err := s.fail[op]; err != nil {
		return err
	}
	return ctx.Err()
}

// batch runs a batch Writer call. prep checks the call as a whole, and an
// error from it fails every ref. each then changes one resolved message
// and returns its ref afterwards.
func (s *Store) batch(ctx context.Context, op string, refs []ref.Ref, prep func() error, each func(*Message) (ref.Ref, error)) []mailstore.RefResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mailstore.RefResult, len(refs))
	err := s.beginWrite(ctx, op)
	if err == nil && prep != nil {
		err = prep()
	}
	for i, r := range refs {
		if err != nil {
			out[i].Err = err
			continue
		}
		m, rerr := s.resolveWriteLocked(r)
		if rerr != nil {
			out[i].Err = rerr
			continue
		}
		out[i].Ref, out[i].Err = each(m)
	}
	return out
}

// resolveWriteLocked finds the message a write names. Unlike reads it does
// not recover a Gmail ref by X-GM-MSGID: a changed UIDVALIDITY is
// ErrStaleRef, and the caller searches again.
func (s *Store) resolveWriteLocked(r ref.Ref) (*Message, error) {
	if r.Account != s.account {
		return nil, fmt.Errorf("%w: ref is for account %s", mailstore.ErrNotFound, r.Account)
	}
	v, ok := s.uidValidity[r.Folder]
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, r.Folder)
	case v != r.UIDValidity:
		return nil, fmt.Errorf("%w: %s changed; search again", mailstore.ErrStaleRef, r.Folder)
	}
	if m := s.findLocked(r.Folder, r.UID); m != nil {
		return m, nil
	}
	return nil, fmt.Errorf("%w: message is gone; search again", mailstore.ErrStaleRef)
}

// folderLocked returns the folder named name.
func (s *Store) folderLocked(name string) (mailstore.Folder, error) {
	for _, f := range s.folders {
		if f.Name == name {
			return f, nil
		}
	}
	return mailstore.Folder{}, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, name)
}

// existsLocked reports whether a folder is called name. INBOX matches in
// any case, as IMAP has it.
func (s *Store) existsLocked(name string) bool {
	if strings.EqualFold(name, "INBOX") {
		return true
	}
	_, ok := s.uidValidity[name]
	return ok
}

// holdsMail reports whether a Gmail folder holds messages itself rather
// than listing All Mail by label.
func holdsMail(role mailstore.Role) bool {
	return role == mailstore.RoleAll || role == mailstore.RoleTrash || role == mailstore.RoleSpam
}

// relocateLocked moves m to folder under a new UID and returns its new ref.
func (s *Store) relocateLocked(m *Message, folder string) ref.Ref {
	m.Folder = folder
	m.uid = s.nextUID[folder]
	s.nextUID[folder]++
	return s.refLocked(m)
}

// allMailLocked is the name of Gmail's All Mail folder.
func (s *Store) allMailLocked() (string, error) {
	if name := s.byRoleLocked(mailstore.RoleAll); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("%w: no All Mail folder", mailstore.ErrNotFound)
}

// roleFolderLocked is the name of the folder with role.
func (s *Store) roleFolderLocked(role mailstore.Role) (string, error) {
	if name := s.byRoleLocked(role); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("%w: no folder has the %s role", mailstore.ErrNotFound, role)
}

// Append implements mailstore.Writer. On Gmail, appending to a label folder
// stores the message in All Mail with that label and returns its All Mail
// ref. A zero date stores the clock's time as the internal date.
func (s *Store) Append(ctx context.Context, folder string, flags []string, date time.Time, raw []byte) (ref.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginWrite(ctx, opAppend); err != nil {
		return ref.Ref{}, err
	}
	f, err := s.folderLocked(folder)
	if err != nil {
		return ref.Ref{}, err
	}
	if err := checkFlags(flags); err != nil {
		return ref.Ref{}, err
	}
	if len(raw) == 0 {
		return ref.Ref{}, errors.New("fake: cannot append an empty message")
	}
	m := parseRaw(raw)
	m.Folder = folder
	m.InternalDate = date
	if date.IsZero() {
		m.InternalDate = s.now()
	}
	m.Flags = editSet(nil, flags, nil)
	if s.gmail {
		if !holdsMail(f.Role) {
			if m.Folder, err = s.allMailLocked(); err != nil {
				return ref.Ref{}, err
			}
			m.Labels = []string{labelOf(f)}
		}
		starFromFlags(&m, flags, nil)
		s.assignGmailIDsLocked(&m)
	}
	m.uid = s.nextUID[m.Folder]
	s.nextUID[m.Folder]++
	s.msgs = append(s.msgs, &m)
	s.appended = append(s.appended, slices.Clone(raw))
	return s.refLocked(&m), nil
}

// parseRaw fills a message's fields from its header, for search and
// summaries. A message whose header does not parse keeps only its bytes.
func parseRaw(raw []byte) Message {
	m := Message{raw: slices.Clone(raw)}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		m.Body = string(raw)
		return m
	}
	h := msg.Header
	addrs := func(key string) []mailstore.Address {
		list, _ := h.AddressList(key) // a bad list is left out, as a summary would
		var out []mailstore.Address
		for _, a := range list {
			out = append(out, mailstore.Address{Name: a.Name, Email: a.Address})
		}
		return out
	}
	m.From, m.To, m.Cc = addrs("From"), addrs("To"), addrs("Cc")
	m.Subject = h.Get("Subject")
	if dec, err := new(mime.WordDecoder).DecodeHeader(m.Subject); err == nil {
		m.Subject = dec
	}
	m.Date, _ = h.Date() // zero when missing or malformed, as dateOf expects
	m.MessageID = h.Get("Message-ID")
	if v := h.Get("In-Reply-To"); v != "" {
		m.InReplyTo = []string{v}
	}
	if v := h.Get("References"); v != "" {
		m.References = []string{v}
	}
	body, _ := io.ReadAll(msg.Body) // reading a bytes.Reader cannot fail
	m.Body = string(body)
	return m
}

// Move implements mailstore.Writer. On standard accounts every move gives a
// new UID, even into the folder the message is in, as UID MOVE does on
// Dovecot (RFC 6851 section 3.3).
func (s *Store) Move(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	var dest mailstore.Folder
	return s.batch(ctx, opMove, refs, func() (err error) {
		dest, err = s.folderLocked(folder)
		return err
	}, func(m *Message) (ref.Ref, error) {
		return s.moveLocked(m, dest)
	})
}

// moveLocked moves m to dest. A standard move always gives a new UID. On
// Gmail a message already in dest keeps its ref, and a label folder is a
// label edit: a message in Trash or Spam goes back to All Mail first, then
// gains the label, and loses \Inbox unless dest is INBOX.
func (s *Store) moveLocked(m *Message, dest mailstore.Folder) (ref.Ref, error) {
	if !s.gmail {
		return s.relocateLocked(m, dest.Name), nil
	}
	if m.Folder == dest.Name {
		return s.refLocked(m), nil
	}
	if holdsMail(dest.Role) {
		return s.relocateLocked(m, dest.Name), nil
	}
	if s.roleLocked(m.Folder) != mailstore.RoleAll {
		all, err := s.allMailLocked()
		if err != nil {
			return ref.Ref{}, err
		}
		s.relocateLocked(m, all)
	}
	label := labelOf(dest)
	m.Labels = editSet(m.Labels, []string{label}, nil)
	if label != `\Inbox` {
		m.Labels = editSet(m.Labels, nil, []string{`\Inbox`})
	}
	return s.refLocked(m), nil
}

// Copy implements mailstore.Writer. On standard accounts the copy is a new
// message with a new ref, even in its own folder. On Gmail copying to a
// label folder adds the label and returns the same ref, and Trash and Spam
// are refused with ErrUnsupported: Gmail keeps one copy of a message.
func (s *Store) Copy(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	var dest mailstore.Folder
	return s.batch(ctx, opCopy, refs, func() (err error) {
		if dest, err = s.folderLocked(folder); err != nil {
			return err
		}
		if s.gmail && (dest.Role == mailstore.RoleTrash || dest.Role == mailstore.RoleSpam) {
			return fmt.Errorf("%w: copy into %s; move or trash instead", mailstore.ErrUnsupported, folder)
		}
		return nil
	}, func(m *Message) (ref.Ref, error) {
		if s.gmail {
			if dest.Role != mailstore.RoleAll {
				m.Labels = editSet(m.Labels, []string{labelOf(dest)}, nil)
			}
			return s.refLocked(m), nil
		}
		c := cloneMessage(*m)
		s.msgs = append(s.msgs, &c)
		return s.relocateLocked(&c, dest.Name), nil
	})
}

// Archive implements mailstore.Writer. Standard accounts move to the
// archive folder: the one SetArchiveFolder names, else the one with the
// archive role, else ErrNotFound. Gmail removes \Inbox and keeps the ref.
func (s *Store) Archive(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	if s.gmail {
		return s.batch(ctx, opArchive, refs, nil, func(m *Message) (ref.Ref, error) {
			m.Labels = editSet(m.Labels, nil, []string{`\Inbox`})
			return s.refLocked(m), nil
		})
	}
	var dest mailstore.Folder
	return s.batch(ctx, opArchive, refs, func() (err error) {
		name := s.archiveFolder
		if name == "" {
			if name, err = s.roleFolderLocked(mailstore.RoleArchive); err != nil {
				return err
			}
		}
		dest, err = s.folderLocked(name)
		return err
	}, func(m *Message) (ref.Ref, error) {
		return s.moveLocked(m, dest)
	})
}

// Trash implements mailstore.Writer: a move to the folder with the trash
// role, [Gmail]/Trash on Gmail. A ref already in Trash succeeds unchanged.
func (s *Store) Trash(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	var dest mailstore.Folder
	return s.batch(ctx, opTrash, refs, func() error {
		name, err := s.roleFolderLocked(mailstore.RoleTrash)
		if err != nil {
			return err
		}
		dest, err = s.folderLocked(name)
		return err
	}, func(m *Message) (ref.Ref, error) {
		if m.Folder == dest.Name {
			return s.refLocked(m), nil // already in Trash: a no-op success (spec 7.1)
		}
		return s.moveLocked(m, dest)
	})
}

// Expunge implements mailstore.Writer. On Gmail a message outside Trash is
// moved to Trash first and expunged there, as Gmail requires.
func (s *Store) Expunge(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	return s.batch(ctx, opExpunge, refs, nil, func(m *Message) (ref.Ref, error) {
		if s.gmail && s.roleLocked(m.Folder) != mailstore.RoleTrash {
			trash, err := s.roleFolderLocked(mailstore.RoleTrash)
			if err != nil {
				return ref.Ref{}, err
			}
			s.relocateLocked(m, trash)
		}
		s.msgs = slices.DeleteFunc(s.msgs, func(o *Message) bool { return o == m })
		return ref.Ref{}, nil
	})
}

// StoreFlags implements mailstore.Writer. It adds, then removes, and keeps
// the ref. Flags compare without case. On Gmail \Flagged is the star, so it
// also adds or removes the \Starred label.
func (s *Store) StoreFlags(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	return s.batch(ctx, opStoreFlags, refs, func() error {
		if err := checkFlags(add); err != nil {
			return err
		}
		return checkFlags(remove)
	}, func(m *Message) (ref.Ref, error) {
		m.Flags = editSet(m.Flags, add, remove)
		if s.gmail {
			starFromFlags(m, add, remove)
		}
		return s.refLocked(m), nil
	})
}

// StoreLabels implements mailstore.Writer. Standard accounts fail every ref
// with ErrUnsupported. On Gmail a label is a system label such as \Inbox
// or \Starred, or the name of an existing label folder in any case, with
// INBOX, Sent Mail and Drafts standing for their system labels. An unknown
// label is ErrNotFound, and All Mail, Trash and Spam are not labels
// (ErrUnsupported). The ref is kept.
func (s *Store) StoreLabels(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	if !s.gmail {
		return s.batch(ctx, opStoreLabels, refs, func() error {
			return fmt.Errorf("%w: labels are Gmail only", mailstore.ErrUnsupported)
		}, nil)
	}
	var adds, removes []string
	return s.batch(ctx, opStoreLabels, refs, func() (err error) {
		if adds, err = s.labelsLocked(add); err != nil {
			return err
		}
		removes, err = s.labelsLocked(remove)
		return err
	}, func(m *Message) (ref.Ref, error) {
		m.Labels = editSet(m.Labels, adds, removes)
		flagFromLabels(m, adds, removes)
		return s.refLocked(m), nil
	})
}

// labelsLocked resolves the labels StoreLabels names.
func (s *Store) labelsLocked(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.HasPrefix(n, `\`) {
			i := slices.IndexFunc(systemLabels, func(l string) bool { return strings.EqualFold(l, n) })
			if i < 0 {
				return nil, fmt.Errorf("%w: no system label %q", mailstore.ErrNotFound, n)
			}
			out = append(out, systemLabels[i])
			continue
		}
		// Gmail label names ignore case.
		i := slices.IndexFunc(s.folders, func(f mailstore.Folder) bool { return strings.EqualFold(f.Name, n) })
		if i < 0 {
			return nil, fmt.Errorf("%w: no label %q", mailstore.ErrNotFound, n)
		}
		f := s.folders[i]
		if holdsMail(f.Role) {
			return nil, fmt.Errorf("%w: %s is not a label; move or trash instead", mailstore.ErrUnsupported, n)
		}
		out = append(out, labelOf(f))
	}
	return out, nil
}

// Gmail's star is both the \Flagged flag and the \Starred label, and
// editing either changes the other.
const (
	starFlag  = `\Flagged`
	starLabel = `\Starred`
)

// starFromFlags follows a flag edit with the \Starred label.
func starFromFlags(m *Message, add, remove []string) {
	m.Labels = editSet(m.Labels, twin(add, starFlag, starLabel), twin(remove, starFlag, starLabel))
}

// flagFromLabels follows a label edit with the \Flagged flag.
func flagFromLabels(m *Message, add, remove []string) {
	m.Flags = editSet(m.Flags, twin(add, starLabel, starFlag), twin(remove, starLabel, starFlag))
}

// twin is [to] when list holds from, ignoring case, else nil.
func twin(list []string, from, to string) []string {
	if slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, from) }) {
		return []string{to}
	}
	return nil
}

// checkFlags refuses what a server refuses in STORE and APPEND: \Recent,
// unknown system flags, and keywords that are not IMAP atoms.
func checkFlags(flags []string) error {
	for _, f := range flags {
		if strings.HasPrefix(f, `\`) {
			if !slices.ContainsFunc(systemFlags, func(x string) bool { return strings.EqualFold(x, f) }) {
				return fmt.Errorf("fake: %q is not a flag a client can store", f)
			}
			continue
		}
		if f == "" || strings.ContainsFunc(f, func(r rune) bool { return r <= ' ' || r >= 0x7f || strings.ContainsRune(`(){%*"]\`, r) }) {
			return fmt.Errorf("fake: %q is not a valid keyword", f)
		}
	}
	return nil
}

// editSet adds then removes flags or labels, ignoring case and keeping one
// of each.
func editSet(list, add, remove []string) []string {
	out := slices.Clone(list)
	for _, a := range add {
		if !slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, a) }) {
			out = append(out, a)
		}
	}
	for _, r := range remove {
		out = slices.DeleteFunc(out, func(x string) bool { return strings.EqualFold(x, r) })
	}
	return out
}

// CreateFolder implements mailstore.Writer. It creates missing parents
// first, splitting on "/". On Gmail it creates labels and refuses names
// under [Gmail]/.
func (s *Store) CreateFolder(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginWrite(ctx, opCreateFolder); err != nil {
		return err
	}
	if err := s.checkNameLocked(name); err != nil {
		return err
	}
	if s.existsLocked(name) {
		return fmt.Errorf("%w: folder %q", mailstore.ErrExists, name)
	}
	s.createWithParentsLocked(name)
	return nil
}

// checkNameLocked refuses an empty name, one with an empty level, one a ref
// cannot carry, and on Gmail any name under [Gmail].
func (s *Store) checkNameLocked(name string) error {
	if name == "" || slices.Contains(strings.Split(name, delimiter), "") {
		return fmt.Errorf("%w: folder name %q", mailstore.ErrRefused, name)
	}
	if !s.refCarriesLocked(name) {
		return fmt.Errorf("%w: folder name %q cannot be carried by a ref", mailstore.ErrRefused, name)
	}
	if s.gmail && isGmailFolder(name) {
		return fmt.Errorf("%w: %s folders belong to Gmail", mailstore.ErrRefused, gmailPrefix)
	}
	return nil
}

func isGmailFolder(name string) bool {
	return name == gmailPrefix || strings.HasPrefix(name, gmailPrefix+delimiter)
}

// createWithParentsLocked adds name and each missing parent, parents first.
func (s *Store) createWithParentsLocked(name string) {
	levels := strings.Split(name, delimiter)
	for i := range levels {
		if p := strings.Join(levels[:i+1], delimiter); !s.existsLocked(p) {
			s.addFolderLocked(p, mailstore.RoleNone, 1, 1)
		}
	}
}

// childrenLocked returns the folders below name, at any depth.
func (s *Store) childrenLocked(name string) []mailstore.Folder {
	var out []mailstore.Folder
	for _, f := range s.folders {
		if strings.HasPrefix(f.Name, name+delimiter) {
			out = append(out, f)
		}
	}
	return out
}

// checkMutableLocked refuses changes to INBOX, role folders and, on Gmail,
// [Gmail] folders.
func (s *Store) checkMutableLocked(f mailstore.Folder) error {
	if strings.EqualFold(f.Name, "INBOX") || f.Role != mailstore.RoleNone || (s.gmail && isGmailFolder(f.Name)) {
		return fmt.Errorf("%w: %s is a system folder", mailstore.ErrRefused, f.Name)
	}
	return nil
}

// RenameFolder implements mailstore.Writer. It renames the folder's
// children too and creates missing parents of to. It refuses INBOX, role
// folders, Gmail's own folders, and a folder moving under itself. A renamed
// folder keeps its messages under new refs, and on Gmail the label is
// renamed on every message, whose refs stay.
func (s *Store) RenameFolder(ctx context.Context, from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginWrite(ctx, opRenameFolder); err != nil {
		return err
	}
	f, err := s.folderLocked(from)
	if err != nil {
		return err
	}
	if err := s.checkMutableLocked(f); err != nil {
		return err
	}
	if err := s.checkNameLocked(to); err != nil {
		return err
	}
	if strings.HasPrefix(to, from+delimiter) {
		return fmt.Errorf("%w: %s cannot move under itself", mailstore.ErrRefused, from)
	}
	moving := append([]mailstore.Folder{f}, s.childrenLocked(from)...)
	renamed := func(name string) string { return to + strings.TrimPrefix(name, from) }
	for _, c := range moving {
		if err := s.checkMutableLocked(c); err != nil {
			return err
		}
		if err := s.checkNameLocked(renamed(c.Name)); err != nil {
			return err // a child's longer name may not fit in a ref
		}
		if s.existsLocked(renamed(c.Name)) {
			return fmt.Errorf("%w: folder %q", mailstore.ErrExists, renamed(c.Name))
		}
	}
	if i := strings.LastIndex(to, delimiter); i > 0 {
		s.createWithParentsLocked(to[:i])
	}
	for _, c := range moving {
		next := renamed(c.Name)
		v, uid := s.uidValidity[c.Name], s.nextUID[c.Name]
		s.removeFolderLocked(c.Name)
		s.addFolderLocked(next, c.Role, v, uid)
		for _, m := range s.msgs {
			if m.Folder == c.Name {
				m.Folder = next
			}
			if s.gmail && hasLabel(m, c.Name) {
				m.Labels = editSet(m.Labels, nil, []string{c.Name})
				m.Labels = append(m.Labels, next)
			}
		}
	}
	return nil
}

// DeleteFolder implements mailstore.Writer. It refuses INBOX, role folders,
// Gmail's own folders, a folder with children, and a folder with mail
// unless force. With force a standard folder's messages are deleted with
// it, and a Gmail label is taken off its messages, which stay in All Mail.
func (s *Store) DeleteFolder(ctx context.Context, name string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginWrite(ctx, opDeleteFolder); err != nil {
		return err
	}
	f, err := s.folderLocked(name)
	if err != nil {
		return err
	}
	if err := s.checkMutableLocked(f); err != nil {
		return err
	}
	if len(s.childrenLocked(name)) > 0 {
		return fmt.Errorf("%w: %s has child folders; delete them first", mailstore.ErrRefused, name)
	}
	held := slices.ContainsFunc(s.msgs, func(m *Message) bool { return s.inFolderLocked(f, m) })
	if held && !force {
		return fmt.Errorf("%w: %s holds mail; use force", mailstore.ErrRefused, name)
	}
	if s.gmail {
		for _, m := range s.msgs {
			m.Labels = editSet(m.Labels, nil, []string{name})
		}
	} else {
		s.msgs = slices.DeleteFunc(s.msgs, func(m *Message) bool { return m.Folder == name })
	}
	s.removeFolderLocked(name)
	return nil
}

// FindByMessageID implements mailstore.Writer. The result is in UID order.
// On Gmail a label folder gives the All Mail refs of the messages with that
// label. An empty Message-ID finds nothing.
func (s *Store) FindByMessageID(ctx context.Context, folder, messageID string) ([]ref.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginWrite(ctx, opFindByMessageID); err != nil {
		return nil, err
	}
	f, err := s.folderLocked(folder)
	if err != nil {
		return nil, err
	}
	id := trimID(messageID)
	if id == "" {
		return nil, nil
	}
	var out []ref.Ref
	for _, m := range s.msgs {
		if s.inFolderLocked(f, m) && trimID(m.MessageID) == id {
			out = append(out, s.refLocked(m))
		}
	}
	slices.SortFunc(out, func(a, b ref.Ref) int {
		return cmp.Or(cmp.Compare(a.Folder, b.Folder), cmp.Compare(a.UID, b.UID))
	})
	return out, nil
}

// refCarriesLocked reports whether every ref into a folder called name
// survives ref.Parse of its String: UTF-8 without control bytes, short
// enough even with the largest UIDs. A server answers anything else with
// BAD, so the fake refuses it rather than hand out refs nobody can use.
func (s *Store) refCarriesLocked(name string) bool {
	probe := ref.Ref{Account: s.account, Folder: name, UIDValidity: math.MaxUint32, UID: math.MaxUint32}
	if s.gmail {
		probe.GmailMsgID = math.MaxUint64
	}
	back, err := ref.Parse(probe.String())
	return err == nil && back == probe
}
