package imapstore

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// ListFolders implements mailstore.Store. It always asks the server and
// refreshes the list searches use.
func (s *Store) ListFolders(ctx context.Context) ([]mailstore.Folder, error) {
	return s.folderList(ctx, true)
}

// folderList returns the cached folder list, or fetches it.
func (s *Store) folderList(ctx context.Context, refresh bool) ([]mailstore.Folder, error) {
	if !refresh {
		s.mu.Lock()
		cached := s.folders
		s.mu.Unlock()
		if cached != nil {
			return cached, nil
		}
	}
	var out []mailstore.Folder
	err := s.withConn(ctx, "", func(c *conn) error {
		var err error
		out, err = c.listFolders(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.folders = out
	s.mu.Unlock()
	return out, nil
}

// forgetFolders drops the cached folder list after a folder turned out missing.
func (s *Store) forgetFolders() {
	s.mu.Lock()
	s.folders = nil
	s.mu.Unlock()
}

func (c *conn) listFolders(ctx context.Context) ([]mailstore.Folder, error) {
	caps := c.client().Caps()
	opts := &imap.ListOptions{}
	statusOpts := &imap.StatusOptions{NumMessages: true, NumUnseen: true}
	if caps.Has(imap.CapListStatus) {
		opts.ReturnStatus = statusOpts
		opts.ReturnSpecialUse = caps.Has(imap.CapSpecialUse)
	}
	var list []*imap.ListData
	if err := c.do(ctx, func() error {
		var err error
		list, err = c.client().List("", "*", opts).Collect()
		return err
	}); err != nil {
		return nil, err
	}
	roles := assignRoles(list)
	folders := make([]mailstore.Folder, len(list))
	var need []int
	for i, l := range list {
		folders[i] = mailstore.Folder{Name: l.Mailbox, Role: roles[i], Selectable: selectable(l.Attrs)}
		switch {
		case l.Status != nil:
			folders[i].Messages, folders[i].Unseen = deref(l.Status.NumMessages), deref(l.Status.NumUnseen)
		case folders[i].Selectable:
			need = append(need, i)
		}
	}
	if len(need) > 0 {
		// STATUS commands are pipelined: all are sent before any answer is read.
		err := c.do(ctx, func() error {
			cmds := make([]*imapclient.StatusCommand, len(need))
			for j, i := range need {
				cmds[j] = c.client().Status(folders[i].Name, statusOpts)
			}
			for j, cmd := range cmds {
				data, err := cmd.Wait()
				var refusal *imap.Error
				switch {
				case errors.As(err, &refusal):
					continue // one folder the server won't count does not fail the list
				case err != nil:
					return err
				}
				f := &folders[need[j]]
				f.Messages, f.Unseen = deref(data.NumMessages), deref(data.NumUnseen)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(folders, func(a, b mailstore.Folder) int {
		if (a.Role == mailstore.RoleInbox) != (b.Role == mailstore.RoleInbox) {
			if a.Role == mailstore.RoleInbox {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return folders, nil
}

func selectable(attrs []imap.MailboxAttr) bool {
	return !slices.Contains(attrs, imap.MailboxAttrNoSelect) && !slices.Contains(attrs, imap.MailboxAttrNonExistent)
}

func deref(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}
