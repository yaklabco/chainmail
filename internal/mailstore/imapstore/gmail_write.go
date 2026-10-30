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

// gmailView is a Gmail account's folders, for its writes.
type gmailView struct {
	folders []mailstore.Folder
	allMail string // "" when All Mail is hidden from IMAP
	trash   string
	// system starts the names of Gmail's own folders: "[Gmail]/", or
	// "[Google Mail]/" on accounts that have that name.
	system string
}

// gmailView returns the account's folders, listed afresh when refresh.
func (s *Store) gmailView(ctx context.Context, refresh bool) (gmailView, error) {
	folders, err := s.folderList(ctx, refresh)
	if err != nil {
		return gmailView{}, err
	}
	v := gmailView{folders: folders, allMail: byRole(folders, mailstore.RoleAll), trash: byRole(folders, mailstore.RoleTrash), system: "[Gmail]/"}
	if i := strings.LastIndex(v.allMail, "/"); i > 0 {
		v.system = v.allMail[:i+1] // Gmail's delimiter is always "/"
	}
	return v, nil
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

// find returns the folder called name. INBOX matches in any case.
func (v gmailView) find(name string) (gmailFolder, bool) {
	i := slices.IndexFunc(v.folders, func(f mailstore.Folder) bool {
		return f.Name == name || (f.Role == mailstore.RoleInbox && strings.EqualFold(name, "INBOX"))
	})
	if i < 0 {
		return gmailFolder{}, false
	}
	return v.classify(v.folders[i]), true
}

// fold returns the folder whose name is name ignoring case, as Gmail
// matches labels.
func (v gmailView) fold(name string) (gmailFolder, bool) {
	i := slices.IndexFunc(v.folders, func(f mailstore.Folder) bool { return strings.EqualFold(f.Name, name) })
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
func (v gmailView) isSystem(name string) bool {
	parent := strings.TrimSuffix(v.system, "/")
	return strings.EqualFold(name, parent) || (len(name) > len(v.system) && strings.EqualFold(name[:len(v.system)], v.system))
}

// labelEdit is a label change applied to every group of a batch.
type labelEdit struct {
	add, remove []string
	// into is the folder a move or copy names: a group already in it is
	// left as it is.
	into string
	// restore says a move: groups in Trash or Spam go back to All Mail
	// first, under new refs, and take the labels there.
	restore bool
}

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
		return s.gmailEdit(ctx, refs, v, labelEdit{into: dest.name})
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
		f, ok := v.fold(n)
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
			for _, uid := range g.uids {
				g.keep(uid)
			}
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

// storeLabels starts the label STOREs of e on set, for pipelined.
func storeLabels(cl *imapclient.Client, set imap.UIDSet, e labelEdit) []func() error {
	var waits []func() error
	if len(e.add) > 0 {
		waits = append(waits, cl.StoreGmailLabels(set, &imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Silent: true, Labels: e.add}, nil).Close)
	}
	if len(e.remove) > 0 {
		waits = append(waits, cl.StoreGmailLabels(set, &imap.StoreGmailLabels{Op: imap.StoreFlagsDel, Silent: true, Labels: e.remove}, nil).Close)
	}
	return waits
}

// labelInPlace edits the group's labels in the folder open on c, pipelined
// after a UID FETCH that learns which messages still exist. When leaves,
// the edit takes them out of that folder, so the FETCH also reads their
// X-GM-MSGIDs, by which their All Mail refs are found afterwards.
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
		return append([]func() error{read}, storeLabels(cl, set, e)...)
	})
	if err != nil {
		return err
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
	return c.pipelined(ctx, func(cl *imapclient.Client) []func() error { return storeLabels(cl, set, e) })
}
