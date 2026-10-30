package imapstore

// Google versions of the Writer methods whose semantics differ there (spec
// 7.1, Google table). Move, Copy, Archive, Trash, Expunge and StoreLabels
// call them first on Google accounts.
//
// Refs live in [Gmail]/All Mail, Trash and Spam. A move or copy into a label
// folder (INBOX, Sent Mail, Drafts or a user label) is a label edit with UID
// STORE X-GM-LABELS and keeps the ref; only moves into or out of Trash and
// Spam give a new one. A ref into a label folder, as Append gives, is edited
// in place too, and when the edit takes the message out of that folder the
// result is its All Mail ref, found by X-GM-MSGID.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Gmail system labels, in the case Gmail writes them.
const (
	labelInbox = `\Inbox`
	labelSent  = `\Sent`
	labelDraft = `\Draft`
)

// gmailSystemLabels are the system labels StoreLabels accepts. \Starred is
// the \Flagged flag as a label.
var gmailSystemLabels = []string{labelInbox, `\Important`, `\Starred`, labelSent, labelDraft}

// gmailKind is what a Gmail folder holds.
type gmailKind int

const (
	gmailLabel gmailKind = iota // INBOX, Sent Mail, Drafts or a user label: messages that carry its label
	gmailAll                    // All Mail: every message outside Trash and Spam
	gmailBin                    // Trash or Spam, which hold their messages themselves
	gmailOther                  // another of Gmail's own folders, such as Starred, with no label known here
)

// gmailFolder is one folder of a Gmail account and the label it lists.
type gmailFolder struct {
	name  string
	kind  gmailKind
	label string // for gmailLabel
}

// systemAttrs are the special-use attributes of Gmail's own folders.
var systemAttrs = []imap.MailboxAttr{
	imap.MailboxAttrAll, imap.MailboxAttrTrash, imap.MailboxAttrJunk, imap.MailboxAttrSent,
	imap.MailboxAttrDrafts, imap.MailboxAttrImportant, imap.MailboxAttrFlagged,
}

// reservedSystem are the names Gmail gives the parent of its own folders.
// They are refused on every Gmail account, whichever one its listing shows.
var reservedSystem = systemSpace{"[Gmail]/", "[Google Mail]/", "[GoogleMail]/"}

// systemSpace is the system namespace of a Gmail account: the parents of
// Gmail's own folders, each with the delimiter after it, such as
// "[Gmail]/". It is found from the folders' special-use attributes rather
// than their names, which some accounts have localised or under
// "[Google Mail]", so it is known while All Mail, or any other of them, is
// hidden from IMAP.
type systemSpace []string

// systemNamespace finds the system namespace in a LIST reply: the parent of
// each folder with one of systemAttrs. A top-level folder with one has no
// namespace and is known by its attribute alone.
func systemNamespace(list []*imap.ListData) systemSpace {
	var out systemSpace
	for _, l := range list {
		if l.Delim == 0 || !hasSystemAttr(l.Attrs) {
			continue
		}
		i := strings.LastIndex(l.Mailbox, string(l.Delim))
		if i <= 0 {
			continue
		}
		p := l.Mailbox[:i+len(string(l.Delim))]
		if !slices.ContainsFunc(out, func(q string) bool { return strings.EqualFold(p, q) }) {
			out = append(out, p)
		}
	}
	return out
}

// hasSystemAttr reports whether attrs mark one of Gmail's own folders.
func hasSystemAttr(attrs []imap.MailboxAttr) bool {
	return slices.ContainsFunc(attrs, func(a imap.MailboxAttr) bool {
		return slices.ContainsFunc(systemAttrs, func(s imap.MailboxAttr) bool { return strings.EqualFold(string(a), string(s)) })
	})
}

// parent returns, without its delimiter, the parent in s or reservedSystem
// that name is or is below, ignoring case.
func (s systemSpace) parent(name string) (string, bool) {
	for _, p := range slices.Concat(s, reservedSystem) {
		_, n := utf8.DecodeLastRuneInString(p)
		top := p[:len(p)-n]
		if strings.EqualFold(name, top) || (len(name) > len(p) && strings.EqualFold(name[:len(p)], p)) {
			return top, true
		}
	}
	return "", false
}

// holds reports whether name is one of Gmail's own folders or their parent.
func (s systemSpace) holds(name string) bool {
	_, ok := s.parent(name)
	return ok
}

// gmailView is a Gmail account's folders, for its writes.
type gmailView struct {
	folders []mailstore.Folder
	allMail string // "" when All Mail is hidden from IMAP
	trash   string
	system  systemSpace
}

// gmailView returns the account's folders, listed afresh when refresh.
func (s *Store) gmailView(ctx context.Context, refresh bool) (gmailView, error) {
	folders, system, err := s.folderListing(ctx, refresh)
	if err != nil {
		return gmailView{}, err
	}
	return gmailView{folders: folders, allMail: byRole(folders, mailstore.RoleAll), trash: byRole(folders, mailstore.RoleTrash), system: system}, nil
}

// gmailDest finds the folder a move or copy names, listing the folders
// again when the cached list lacks it.
func (s *Store) gmailDest(ctx context.Context, name string) (gmailView, gmailFolder, error) {
	for _, refresh := range []bool{false, true} {
		v, err := s.gmailView(ctx, refresh)
		if err != nil {
			return gmailView{}, gmailFolder{}, err
		}
		if f, ok := v.find(name); ok {
			return v, f, nil
		}
	}
	return gmailView{}, gmailFolder{}, fmt.Errorf("%w: no folder %q", mailstore.ErrNotFound, name)
}

// find returns the folder called name, or else the one whose name is name
// ignoring case, as Gmail matches labels and INBOX.
func (v gmailView) find(name string) (gmailFolder, bool) {
	i := slices.IndexFunc(v.folders, func(f mailstore.Folder) bool { return f.Name == name })
	if i < 0 {
		i = slices.IndexFunc(v.folders, func(f mailstore.Folder) bool { return strings.EqualFold(f.Name, name) })
	}
	if i < 0 {
		return gmailFolder{}, false
	}
	return v.classify(v.folders[i]), true
}

// source classifies the folder a group's refs name. One the list lacks,
// such as a label made since it was read, is taken for a user label.
func (v gmailView) source(name string) gmailFolder {
	if f, ok := v.find(name); ok {
		return f
	}
	return gmailFolder{name: name, kind: gmailLabel, label: name}
}

func (v gmailView) classify(f mailstore.Folder) gmailFolder {
	out := gmailFolder{name: f.Name, kind: gmailLabel, label: f.Name}
	switch f.Role {
	case mailstore.RoleInbox:
		out.label = labelInbox
	case mailstore.RoleSent:
		out.label = labelSent
	case mailstore.RoleDrafts:
		out.label = labelDraft
	case mailstore.RoleAll:
		out.kind, out.label = gmailAll, ""
	case mailstore.RoleTrash, mailstore.RoleSpam:
		out.kind, out.label = gmailBin, ""
	default:
		// A role guessed from the name, such as a label called Archive, is
		// still a user label; Gmail's own folders all sit under system.
		if v.isSystem(f.Name) {
			out.kind, out.label = gmailOther, ""
		}
	}
	return out
}

// isSystem reports whether name is one of Gmail's own folders or their parent.
func (v gmailView) isSystem(name string) bool { return v.system.holds(name) }

// labelEdit is a label change applied to every group of a batch.
type labelEdit struct {
	add, remove []string
	// into is the folder a move or copy names: a group already in it is
	// left as it is, once a UID FETCH has found which messages are there.
	into string
	// restore says a move: groups in Trash or Spam go back to All Mail
	// first, under new refs, and take the labels there.
	restore bool
	// binErr, when set, fails a group in Trash or Spam without a command.
	binErr error
}

// errBinNotInAllMail refuses a copy into All Mail of a message in Trash or
// Spam, which is not in All Mail: Gmail keeps one copy of a message, so
// only a move takes it there.
var errBinNotInAllMail = fmt.Errorf("%w: the message is in Trash or Spam, not All Mail, and Gmail keeps one copy of a message; move it to All Mail instead", mailstore.ErrUnsupported)

// errRemovalFailed reports a label edit whose additions went through but
// whose removals did not, or may not have.
var errRemovalFailed = fmt.Errorf("%w: the labels were added, but removing the others failed, so the message may still be in its folder; search before trying again", mailstore.ErrUnavailable)

func (s *Store) gmailMove(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	v, dest, err := s.gmailDest(ctx, folder)
	if err != nil {
		return failAll(refs, err)
	}
	switch dest.kind {
	case gmailAll, gmailBin:
		// UID MOVE: into Trash or Spam, or back to All Mail. A ref already
		// there is left as it is.
		return s.transfer(ctx, refs, dest.name, false)
	case gmailOther:
		return failAll(refs, otherFolder(dest.name))
	}
	e := labelEdit{add: []string{dest.label}, into: dest.name, restore: true}
	if dest.label != labelInbox {
		e.remove = []string{labelInbox}
	}
	return s.gmailEdit(ctx, refs, v, e)
}

func (s *Store) gmailCopy(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	v, dest, err := s.gmailDest(ctx, folder)
	if err != nil {
		return failAll(refs, err)
	}
	switch dest.kind {
	case gmailBin:
		return failAll(refs, fmt.Errorf("%w: Gmail keeps one copy of a message, so it cannot be copied into %s; move or trash it instead", mailstore.ErrUnsupported, dest.name))
	case gmailOther:
		return failAll(refs, otherFolder(dest.name))
	case gmailAll:
		// Every message outside Trash and Spam is in All Mail already.
		return s.gmailEdit(ctx, refs, v, labelEdit{into: dest.name, binErr: errBinNotInAllMail})
	}
	return s.gmailEdit(ctx, refs, v, labelEdit{add: []string{dest.label}, into: dest.name})
}

func otherFolder(name string) error {
	return fmt.Errorf("%w: %s is one of Gmail's own folders; use StoreLabels or StoreFlags for the label it shows", mailstore.ErrUnsupported, name)
}

func (s *Store) gmailArchive(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	v, err := s.gmailView(ctx, false)
	if err != nil {
		return failAll(refs, err)
	}
	return s.gmailEdit(ctx, refs, v, labelEdit{remove: []string{labelInbox}})
}

func (s *Store) gmailTrash(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	trash, err := s.roleFolder(ctx, mailstore.RoleTrash)
	if err != nil {
		return failAll(refs, err)
	}
	return s.transfer(ctx, refs, trash, false)
}

// gmailExpunge moves each message to Trash, unless it is there, then marks
// it \Deleted and expunges it there by UID: Gmail deletes for good only
// from Trash.
func (s *Store) gmailExpunge(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	trash, err := s.roleFolder(ctx, mailstore.RoleTrash)
	if err != nil {
		return failAll(refs, err)
	}
	return s.writeBatch(ctx, refs, func(ctx context.Context, c *conn, g *groupWrite, sent func()) error {
		if c.Selected() == trash {
			return expungeGroup(ctx, c, g, sent)
		}
		sent()
		cu, err := c.uidMove(ctx, uidSet(g.uids), trash)
		if err != nil {
			return s.destError(err, trash)
		}
		pairs, ok := cu.pairs(len(g.uids))
		if cu.validity == 0 || !ok {
			g.failAll(fmt.Errorf("%w: %s moved the messages to Trash without saying where, so they were not expunged; they are in Trash", mailstore.ErrUnavailable, s.acct.Name))
			return nil
		}
		if len(pairs) == 0 {
			return nil // all gone already
		}
		if err := s.expungeMoved(ctx, c, trash, cu.validity, pairs); err != nil {
			s.opts.Logger.Debug("expunging in Trash", "account", s.acct.Name, "err", err)
			kept := fmt.Errorf("%w: %s moved the message to Trash, but expunging it there failed; it is in Trash", mailstore.ErrUnavailable, s.acct.Name)
			for src := range pairs {
				g.set(src, mailstore.RefResult{Err: kept})
			}
			return nil
		}
		for src := range pairs {
			g.set(src, mailstore.RefResult{})
		}
		return nil
	})
}

// expungeMoved opens trash read-write on c and expunges the UIDs pairs moved
// there, unless its UIDVALIDITY is no longer the one COPYUID gave.
func (s *Store) expungeMoved(ctx context.Context, c *conn, trash string, validity uint32, pairs map[uint32]uint32) error {
	if err := s.openMode(ctx, c, trash, true); err != nil {
		return err
	}
	if c.uidValidity != validity {
		return fmt.Errorf("%s changed UIDVALIDITY after the move", trash)
	}
	return c.expungeUIDs(ctx, uidSet(slices.Sorted(maps.Values(pairs))))
}

func (s *Store) gmailStoreLabels(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	v, err := s.gmailView(ctx, false)
	if err != nil {
		return failAll(refs, err)
	}
	adds, created, err := v.labels(add)
	if err != nil {
		return failAll(refs, err)
	}
	removes, _, err := v.labels(remove)
	if err != nil {
		return failAll(refs, err)
	}
	out := s.gmailEdit(ctx, refs, v, labelEdit{add: adds, remove: removes})
	if created {
		s.forgetFolders() // Gmail made a folder for each new label
	}
	return out
}

// labels turns label names into what STORE X-GM-LABELS takes: system
// labels in Gmail's case, the folder names INBOX, Sent Mail and Drafts as
// their system labels, and other names as they are. A name no folder has
// is a new label, which Gmail creates, and created reports one. All Mail,
// Trash, Spam and Gmail's other folders are not labels.
func (v gmailView) labels(names []string) (out []string, created bool, err error) {
	out = make([]string, 0, len(names))
	for _, n := range names {
		switch {
		case n == "" || !utf8.ValidString(n) || strings.ContainsFunc(n, unicode.IsControl):
			return nil, false, fmt.Errorf("%w: a label must be non-empty text without control characters", mailstore.ErrUnsupported)
		case strings.HasPrefix(n, `\`):
			i := slices.IndexFunc(gmailSystemLabels, func(l string) bool { return strings.EqualFold(l, n) })
			if i < 0 {
				return nil, false, fmt.Errorf("%w: no system label %q", mailstore.ErrNotFound, n)
			}
			out = append(out, gmailSystemLabels[i])
			continue
		}
		f, ok := v.find(n)
		switch {
		case !ok && v.isSystem(n):
			return nil, false, fmt.Errorf("%w: %s is one of Gmail's own folders, not a label", mailstore.ErrUnsupported, n)
		case !ok:
			out, created = append(out, n), true
		case f.kind == gmailLabel:
			out = append(out, f.label)
		default:
			return nil, false, fmt.Errorf("%w: %s is not a label; move or trash instead", mailstore.ErrUnsupported, f.name)
		}
	}
	return out, created, nil
}

// gmailEdit applies e to refs, group by group. A group in a label folder or
// All Mail is edited where it is. With e.restore, a group in Trash or Spam
// goes back to All Mail first.
func (s *Store) gmailEdit(ctx context.Context, refs []ref.Ref, v gmailView, e labelEdit) []mailstore.RefResult {
	return s.writeBatch(ctx, refs, func(ctx context.Context, c *conn, g *groupWrite, sent func()) error {
		src := v.source(c.Selected())
		switch {
		case e.into != "" && src.name == e.into:
			return keepPresent(ctx, c, g)
		case e.binErr != nil && src.kind == gmailBin:
			g.failAll(e.binErr)
			return nil
		case e.restore && src.kind == gmailBin:
			return s.restoreThenLabel(ctx, c, g, v.allMail, e, sent)
		}
		// Removing the label of the folder the refs name takes the
		// messages out of it.
		leaves := (src.kind == gmailLabel && slices.ContainsFunc(e.remove, func(l string) bool { return strings.EqualFold(l, src.label) })) ||
			(src.kind == gmailOther && len(e.remove) > 0)
		return s.labelInPlace(ctx, c, g, v.allMail, e, leaves, sent)
	})
}

// addLabels starts the label additions of e on set, for pipelined.
func addLabels(cl *imapclient.Client, set imap.UIDSet, e labelEdit) []func() error {
	if len(e.add) == 0 {
		return nil
	}
	return []func() error{cl.StoreGmailLabels(set, &imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Silent: true, Labels: e.add}, nil).Close}
}

// removeLabels sends the label removals of e on set and waits for the
// answer. It runs only once the additions are confirmed: a removal
// pipelined after a refused addition would still go through, and a Move's
// -\Inbox would take the message out of INBOX without its new label.
func (c *conn) removeLabels(ctx context.Context, set imap.UIDSet, e labelEdit) error {
	if len(e.remove) == 0 {
		return nil
	}
	return c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		return []func() error{cl.StoreGmailLabels(set, &imap.StoreGmailLabels{Op: imap.StoreFlagsDel, Silent: true, Labels: e.remove}, nil).Close}
	})
}

// labelInPlace edits the group's labels in the folder open on c. The
// additions are pipelined after a UID FETCH that learns which messages
// still exist, and the removals follow for those messages once the server
// has accepted the additions. When leaves, the edit takes the messages out
// of that folder, so the FETCH also reads their X-GM-MSGIDs, by which their
// All Mail refs are found afterwards.
func (s *Store) labelInPlace(ctx context.Context, c *conn, g *groupWrite, allMail string, e labelEdit, leaves bool, sent func()) error {
	set := uidSet(g.uids)
	var bufs []*imapclient.FetchMessageBuffer
	sent()
	err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error {
		fetch := cl.Fetch(set, &imap.FetchOptions{UID: true, GmailMsgID: leaves})
		read := func() error {
			var err error
			bufs, err = fetch.Collect()
			return err
		}
		return append([]func() error{read}, addLabels(cl, set, e)...)
	})
	if err != nil {
		return err
	}
	present := make([]uint32, 0, len(bufs))
	for _, b := range bufs {
		present = append(present, uint32(b.UID))
	}
	slices.Sort(present)
	if len(present) > 0 {
		if err := c.removeLabels(ctx, uidSet(present), e); err != nil {
			// A broken connection has been dropped from the pool already;
			// either way the removal may not have happened.
			s.opts.Logger.Debug("removing labels after adding them", "account", s.acct.Name, "err", err)
			for _, uid := range present {
				g.set(uid, mailstore.RefResult{Err: errRemovalFailed})
			}
			return nil
		}
	}
	if !leaves {
		for _, b := range bufs {
			g.keep(uint32(b.UID))
		}
		return nil
	}
	ids := make(map[uint32]uint64, len(bufs))
	for _, b := range bufs {
		ids[uint32(b.UID)] = b.GmailMsgID
	}
	s.placeInAllMail(ctx, c, g, allMail, ids)
	return nil
}

// placeInAllMail records, for messages that left the label folder their
// refs name, their All Mail refs, found by X-GM-MSGID. The change is done,
// so a message that cannot be found gets a zero ref, place unknown, rather
// than an error.
func (s *Store) placeInAllMail(ctx context.Context, c *conn, g *groupWrite, allMail string, ids map[uint32]uint64) {
	found := map[uint32]ref.Ref{}
	if allMail != "" {
		if err := s.open(ctx, c, allMail); err != nil {
			s.opts.Logger.Debug("opening All Mail to find edited messages", "account", s.acct.Name, "err", err)
		} else {
			for _, src := range slices.Sorted(maps.Keys(ids)) {
				uid, err := c.findMsgID(ctx, ids[src])
				if err != nil {
					s.opts.Logger.Debug("finding an edited message in All Mail", "account", s.acct.Name, "err", err)
					break
				}
				if uid != 0 {
					found[src] = ref.Ref{Account: s.acct.Name, Folder: allMail, UIDValidity: c.uidValidity, UID: uid, GmailMsgID: ids[src]}
				}
			}
		}
	}
	for src := range ids {
		g.set(src, mailstore.RefResult{Ref: found[src]})
	}
}

// restoreThenLabel moves a group in Trash or Spam back to All Mail with UID
// MOVE, then applies e's labels there. The results are the All Mail refs
// COPYUID gives, keeping each ref's X-GM-MSGID. Once the move is done, a
// failure is reported per message rather than as a failure of the group.
func (s *Store) restoreThenLabel(ctx context.Context, c *conn, g *groupWrite, allMail string, e labelEdit, sent func()) error {
	if allMail == "" {
		g.failAll(fmt.Errorf("%w: no All Mail folder; in Gmail settings, show All Mail in IMAP", mailstore.ErrNotFound))
		return nil
	}
	sent()
	cu, err := c.uidMove(ctx, uidSet(g.uids), allMail)
	if err != nil {
		return s.destError(err, allMail)
	}
	pairs, ok := cu.pairs(len(g.uids))
	if cu.validity == 0 || !ok {
		g.failAll(fmt.Errorf("%w: %s moved the messages to All Mail without saying where, so the label was not applied; search again", mailstore.ErrUnavailable, s.acct.Name))
		return nil
	}
	if len(pairs) == 0 {
		return nil // all gone already
	}
	if err := s.labelMoved(ctx, c, allMail, cu.validity, pairs, e); err != nil {
		s.opts.Logger.Debug("labelling messages moved to All Mail", "account", s.acct.Name, "err", err)
		half := fmt.Errorf("%w: %s moved the message back to All Mail, but the label change failed; search again", mailstore.ErrUnavailable, s.acct.Name)
		for src := range pairs {
			g.set(src, mailstore.RefResult{Err: half})
		}
		return nil
	}
	for src, dst := range pairs {
		g.place(src, ref.Ref{Account: s.acct.Name, Folder: allMail, UIDValidity: cu.validity, UID: dst})
	}
	return nil
}

// labelMoved opens allMail read-write on c and applies e to the UIDs pairs
// moved there, unless its UIDVALIDITY is no longer the one COPYUID gave.
func (s *Store) labelMoved(ctx context.Context, c *conn, allMail string, validity uint32, pairs map[uint32]uint32, e labelEdit) error {
	if err := s.openMode(ctx, c, allMail, true); err != nil {
		return err
	}
	if c.uidValidity != validity {
		return fmt.Errorf("%s changed UIDVALIDITY after the move", allMail)
	}
	set := uidSet(slices.Sorted(maps.Values(pairs)))
	if err := c.pipelined(ctx, func(cl *imapclient.Client) []func() error { return addLabels(cl, set, e) }); err != nil {
		return err
	}
	return c.removeLabels(ctx, set, e)
}
