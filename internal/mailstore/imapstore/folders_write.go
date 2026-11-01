package imapstore

// Folder changes: CREATE, RENAME and DELETE after the checks of spec 7.1.
// Each lists the folders first, for the server's hierarchy delimiter, the
// folders that exist and their roles. On Gmail, folders are labels, names
// match without regard to case, and Gmail's own folders, known by their
// special-use attributes and the namespace they sit in, are refused.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// CreateFolder implements mailstore.Writer. The name is written as the
// server has it (serverName), and missing parents are created first, split
// on the server's hierarchy delimiter.
func (s *Store) CreateFolder(ctx context.Context, name string) (string, error) {
	var created string
	err := s.changeFolders(ctx, false, func(c *conn, t folderTree, sent func()) error {
		name := t.serverName(name)
		if err := t.checkName(s.acct.Name, name); err != nil {
			return err
		}
		if t.find(name) >= 0 {
			return fmt.Errorf("%w: folder %q", mailstore.ErrExists, name)
		}
		sent()
		if err := t.createParents(ctx, c, name); err != nil {
			return err
		}
		if err := c.folderCommand(ctx, name, func() error { return c.client().Create(name, nil).Wait() }); err != nil {
			return err
		}
		created = name
		return nil
	})
	return created, err
}

// RenameFolder implements mailstore.Writer. It refuses INBOX, role folders
// and Gmail's own folders, as the source or among its children, and a
// folder moving under itself. from is found as given, else as serverName
// writes it. to is written as the server has it (serverName), and its
// missing parents are created first. The server renames the children with
// the folder.
func (s *Store) RenameFolder(ctx context.Context, from, to string) (string, error) {
	var renamed string
	err := s.changeFolders(ctx, true, func(c *conn, t folderTree, sent func()) error {
		to := t.serverName(to)
		i := t.lookup(from)
		if i < 0 {
			return &mailstore.FolderNotFoundError{Name: t.serverName(from)}
		}
		if err := t.mutable(i); err != nil {
			return err
		}
		if err := t.checkName(s.acct.Name, to); err != nil {
			return err
		}
		src := t.list[i].Mailbox
		// On Gmail, a change of case alone renames the label.
		if j := t.find(to); j >= 0 && (j != i || to == src) {
			return fmt.Errorf("%w: folder %q", mailstore.ErrExists, to)
		}
		if t.under(to, src) {
			return fmt.Errorf("%w: %s cannot move under itself", mailstore.ErrRefused, src)
		}
		moving := t.children(src)
		for _, k := range moving {
			if err := t.mutable(k); err != nil {
				return err
			}
			next := to + t.list[k].Mailbox[len(src):]
			if err := t.checkName(s.acct.Name, next); err != nil {
				return err // a child's longer name may not fit in a ref
			}
			if j := t.find(next); j >= 0 && j != i && !slices.Contains(moving, j) {
				return fmt.Errorf("%w: folder %q", mailstore.ErrExists, next)
			}
		}
		sent()
		if err := t.createParents(ctx, c, to); err != nil {
			return err
		}
		if err := c.folderCommand(ctx, src, func() error { return c.client().Rename(src, to, nil).Wait() }); err != nil {
			return err
		}
		renamed = to
		return nil
	})
	return renamed, err
}

// DeleteFolder implements mailstore.Writer. It refuses INBOX, role folders,
// Gmail's own folders and a folder with folders under it, and, unless force,
// one STATUS counts mail in. name is found as given, else as serverName
// writes it. Deleting a Gmail label leaves its messages in All Mail.
func (s *Store) DeleteFolder(ctx context.Context, name string, force bool) error {
	return s.changeFolders(ctx, true, func(c *conn, t folderTree, sent func()) error {
		i := t.lookup(name)
		if i < 0 {
			return &mailstore.FolderNotFoundError{Name: t.serverName(name)}
		}
		if err := t.mutable(i); err != nil {
			return err
		}
		target := t.list[i].Mailbox
		if len(t.children(target)) > 0 {
			return fmt.Errorf("%w: %s has folders under it; delete them first", mailstore.ErrRefused, target)
		}
		if !force && selectable(t.list[i].Attrs) {
			n, err := c.messageCount(ctx, target)
			if err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%w: %s holds %d messages; delete with force to remove them too", mailstore.ErrRefused, target, n)
			}
		}
		sent()
		return c.folderCommand(ctx, target, func() error { return c.client().Delete(target).Wait() })
	})
}

// changeFolders runs f on a pooled connection with the folders listed. f
// calls sent just before its first command that changes anything, and from
// then on the cached folder list is dropped. A rename or delete (moves)
// also makes every connection open its folder again before using it.
func (s *Store) changeFolders(ctx context.Context, moves bool, f func(c *conn, t folderTree, sent func()) error) error {
	changed := false
	err := s.writeConn(ctx, "", false, func(c *conn, sent func()) error {
		t, err := c.folderTree(ctx, s.gmail())
		if err != nil {
			return err
		}
		// Gmail keeps its labels at the top level.
		if !s.gmail() {
			ns, err := s.namespace(ctx, c)
			if err != nil {
				return err
			}
			t.prefix = ns.prefix
		}
		return f(c, t, func() {
			changed = true
			sent()
		})
	})
	if changed {
		if moves {
			s.folderGen.Add(1)
		}
		s.forgetFolders()
	}
	return err
}

// folderTree is the account's folders as LIST gives them.
type folderTree struct {
	list  []*imap.ListData
	roles []mailstore.Role
	// delim is the hierarchy delimiter, or 0 on a server with flat names.
	delim rune
	gmail bool
	// system is Gmail's system namespace, found by special-use attribute.
	system systemSpace
	// prefix is the personal namespace prefix, such as "INBOX.", that the
	// server keeps every folder under, or "" when it keeps them at the top.
	prefix string
}

// folderSpace is how the server names folders, for a name given without a
// LIST at hand: its personal namespace prefix and hierarchy delimiter.
type folderSpace struct {
	prefix string
	delim  rune // 0 on a server with flat names
}

// namespace returns the account's folderSpace. The first call asks the
// server on c: NAMESPACE gives the personal prefix and its delimiter.
// Without NAMESPACE, or when the server refuses it or has no personal
// namespace, the prefix is "" and LIST "" "" gives the delimiter. The store
// keeps the answer for its whole life, since a server does not move its
// namespaces.
func (s *Store) namespace(ctx context.Context, c *conn) (folderSpace, error) {
	s.mu.Lock()
	ns, known := s.ns, s.nsKnown
	s.mu.Unlock()
	if known {
		return ns, nil
	}
	found := false
	if c.client().Caps().Has(imap.CapNamespace) {
		var data *imap.NamespaceData
		err := c.do(ctx, func() error {
			var err error
			data, err = c.client().Namespace().Wait()
			return err
		})
		var refusal *imap.Error
		switch {
		case errors.As(err, &refusal):
		case err != nil:
			return folderSpace{}, err
		case len(data.Personal) > 0:
			ns, found = folderSpace{prefix: data.Personal[0].Prefix, delim: data.Personal[0].Delim}, true
		}
	}
	if !found {
		d, err := c.hierarchyDelim(ctx)
		if err != nil {
			return folderSpace{}, err
		}
		ns.delim = d
	}
	s.mu.Lock()
	s.ns, s.nsKnown = ns, true
	s.mu.Unlock()
	return ns, nil
}

// hierarchyDelim asks LIST "" "" for the server's hierarchy delimiter, or 0
// on a server with flat names.
func (c *conn) hierarchyDelim(ctx context.Context) (rune, error) {
	var list []*imap.ListData
	if err := c.do(ctx, func() error {
		var err error
		list, err = c.client().List("", "", nil).Collect()
		return err
	}); err != nil {
		return 0, err
	}
	for _, l := range list {
		if l.Delim != 0 {
			return l.Delim, nil
		}
	}
	return 0, nil
}

// serverName is name as the server has it: "/" written as the server's
// hierarchy delimiter, and, unless name is INBOX or starts with the
// personal namespace prefix already, under that prefix, as Dovecot servers
// such as Hostinger's keep every folder under "INBOX.".
func (t folderTree) serverName(name string) string {
	if t.delim != 0 && t.delim != '/' {
		name = strings.ReplaceAll(name, "/", string(t.delim))
	}
	if t.prefix == "" || strings.EqualFold(name, "INBOX") || t.inNamespace(name) {
		return name
	}
	return t.prefix + name
}

// inNamespace reports whether name starts with the personal namespace
// prefix, INBOX in it matched in any case, as IMAP matches INBOX.
func (t folderTree) inNamespace(name string) bool {
	n := len(t.prefix)
	if len(name) < n {
		return false
	}
	head := name[:n]
	if head == t.prefix {
		return true
	}
	const inbox = "INBOX"
	return n > len(inbox) && strings.EqualFold(t.prefix[:len(inbox)], inbox) &&
		strings.EqualFold(head[:len(inbox)], inbox) && head[len(inbox):] == t.prefix[len(inbox):]
}

// folderTree lists every folder, with special-use attributes where the
// server can return them, for their roles.
func (c *conn) folderTree(ctx context.Context, gmail bool) (folderTree, error) {
	caps := c.client().Caps()
	opts := &imap.ListOptions{ReturnSpecialUse: caps.Has(imap.CapSpecialUse) && (caps.Has(imap.CapListExtended) || caps.Has(imap.CapListStatus))}
	var list []*imap.ListData
	if err := c.do(ctx, func() error {
		var err error
		list, err = c.client().List("", "*", opts).Collect()
		return err
	}); err != nil {
		return folderTree{}, err
	}
	t := folderTree{list: list, roles: assignRoles(list), gmail: gmail}
	for _, l := range list {
		if l.Delim != 0 {
			t.delim = l.Delim
			break
		}
	}
	if gmail {
		t.system = systemNamespace(list)
	}
	return t, nil
}

// same reports whether a and b name one folder: INBOX in any case, and on
// Gmail any name without regard to case, as Gmail matches labels.
func (t folderTree) same(a, b string) bool {
	if a == b {
		return true
	}
	return (t.gmail || strings.EqualFold(a, "INBOX")) && strings.EqualFold(a, b)
}

// find returns the index of the folder called name, or -1.
func (t folderTree) find(name string) int {
	return slices.IndexFunc(t.list, func(l *imap.ListData) bool { return t.same(l.Mailbox, name) })
}

// lookup returns the index of the folder a caller names: the one called
// name, else the one called serverName(name), so a name given without the
// personal namespace prefix, or with "/" between levels, finds the server's
// folder. It returns -1 when neither exists.
func (t folderTree) lookup(name string) int {
	if i := t.find(name); i >= 0 {
		return i
	}
	return t.find(t.serverName(name))
}

// under reports whether name is below parent in the hierarchy.
func (t folderTree) under(name, parent string) bool {
	if t.delim == 0 {
		return false
	}
	prefix := parent + string(t.delim)
	return len(name) > len(prefix) && t.same(name[:len(prefix)], prefix)
}

// children returns the indexes of the folders below name, at any depth.
func (t folderTree) children(name string) []int {
	var out []int
	for i, l := range t.list {
		if t.under(l.Mailbox, name) {
			out = append(out, i)
		}
	}
	return out
}

// systemParent returns, on Gmail, the system namespace name is in or is
// the parent of.
func (t folderTree) systemParent(name string) (string, bool) {
	if !t.gmail {
		return "", false
	}
	return t.system.parent(name)
}

// checkName refuses a name no folder can have here: empty, with an empty
// level, one a ref cannot carry, or on Gmail one of Gmail's own.
func (t folderTree) checkName(account, name string) error {
	if name == "" || (t.delim != 0 && slices.Contains(strings.Split(name, string(t.delim)), "")) {
		return fmt.Errorf("%w: %q is not a folder name; each level needs a name", mailstore.ErrRefused, name)
	}
	if !refCarries(account, name, t.gmail) {
		return fmt.Errorf("%w: folder name %q cannot be carried by a ref", mailstore.ErrRefused, name)
	}
	if parent, ok := t.systemParent(name); ok {
		return fmt.Errorf("%w: %s folders belong to Gmail", mailstore.ErrRefused, parent)
	}
	return nil
}

// refCarries reports whether every ref into a folder called name survives
// ref.Parse of its String, even with the largest UIDs.
func refCarries(account, name string, gmail bool) bool {
	probe := ref.Ref{Account: account, Folder: name, UIDValidity: math.MaxUint32, UID: math.MaxUint32}
	if gmail {
		probe.GmailMsgID = math.MaxUint64
	}
	back, err := ref.Parse(probe.String())
	return err == nil && back == probe
}

// mutable refuses a change to INBOX, a role folder or one of Gmail's own.
// On Gmail a role folder is one with a special-use attribute of Gmail's
// own folders, or anything in their namespace; a label whose name suggests
// a role, such as Archive, is still the user's.
func (t folderTree) mutable(i int) error {
	name := t.list[i].Mailbox
	var role bool
	if t.gmail {
		_, inSystem := t.systemParent(name)
		role = inSystem || hasSystemAttr(t.list[i].Attrs)
	} else {
		role = t.roles[i] != mailstore.RoleNone
	}
	if strings.EqualFold(name, "INBOX") || role {
		return fmt.Errorf("%w: %s is a system folder", mailstore.ErrRefused, name)
	}
	return nil
}

// createParents creates each missing folder above name, top first. One
// that turns out to exist already is fine.
func (t folderTree) createParents(ctx context.Context, c *conn, name string) error {
	if t.delim == 0 {
		return nil
	}
	levels := strings.Split(name, string(t.delim))
	for n := 1; n < len(levels); n++ {
		parent := strings.Join(levels[:n], string(t.delim))
		if t.find(parent) >= 0 {
			continue
		}
		err := c.folderCommand(ctx, parent, func() error { return c.client().Create(parent, nil).Wait() })
		if err != nil && !errors.Is(err, mailstore.ErrExists) {
			return err
		}
	}
	return nil
}

// messageCount asks STATUS how many messages folder holds.
func (c *conn) messageCount(ctx context.Context, folder string) (uint32, error) {
	var data *imap.StatusData
	err := c.do(ctx, func() error {
		var err error
		data, err = c.client().Status(folder, &imap.StatusOptions{NumMessages: true}).Wait()
		return err
	})
	if err != nil {
		return 0, folderError(err, folder)
	}
	return deref(data.NumMessages), nil
}

// folderCommand runs a CREATE, RENAME or DELETE of name and sorts the
// server's refusal.
func (c *conn) folderCommand(ctx context.Context, name string, f func() error) error {
	return folderError(c.do(ctx, f), name)
}

// folderError turns a NO to a folder command into the mailstore error it
// means, leaving the server's text out. Other failures pass through.
func folderError(err error, name string) error {
	var e *imap.Error
	if !errors.As(err, &e) || e.Type != imap.StatusResponseTypeNo {
		return err
	}
	switch {
	case e.Code == imap.ResponseCodeAlreadyExists || strings.Contains(strings.ToLower(e.Text), "already exists"):
		return fmt.Errorf("%w: folder %q", mailstore.ErrExists, name)
	case missingFolder(err):
		return &mailstore.FolderNotFoundError{Name: name}
	case e.Code == imap.ResponseCodeCannot || e.Code == imap.ResponseCodeNoPerm:
		return fmt.Errorf("%w: the server refused to change %q", mailstore.ErrRefused, name)
	}
	return err
}
