package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

const (
	// maxOrganizeRefs is the most refs one organize call takes.
	maxOrganizeRefs = 500
	// organizeTimeout is the deadline of an organize call whose client set
	// none. A batch of 500 refs can span many folders on several accounts.
	organizeTimeout = 2 * time.Minute
	// maxFolderBytes is the longest folder or label name accepted.
	maxFolderBytes = 255
	// maxAccountFanOut is the most accounts one organize call works on at
	// once. Each account's store bounds its own folder groups by its pool.
	maxAccountFanOut = 8
)

// storableFlags are the system flags a client may add or remove. \Deleted
// is refused because expunge is the only way to delete, and \Recent is the
// server's own.
var storableFlags = []string{`\Seen`, `\Flagged`, `\Answered`, `\Draft`}

// keywordRE matches the keywords a client may add or remove.
var keywordRE = regexp.MustCompile(`^[A-Za-z0-9$_.-]{1,64}$`)

// organizeService moves, copies, flags, labels and deletes messages, and
// manages folders.
type organizeService struct {
	chainmailv1connect.UnimplementedOrganizeServiceHandler
	s *Server
}

var _ chainmailv1connect.OrganizeServiceHandler = (*organizeService)(nil)

// batchCall is one batch organize call.
type batchCall struct {
	action string
	refs   []string
	// check validates the call's arguments other than refs. An error
	// refuses the whole call before any account is touched.
	check func() error
	// targets follow each account's refs in its audit row, such as the
	// destination folder.
	targets []string
	// run makes the change on one account's refs.
	run func(ctx context.Context, w mailstore.Writer, refs []ref.Ref) []mailstore.RefResult
}

// refGroup is the refs of one batch that name one account.
type refGroup struct {
	account string
	// idx are the refs' positions in the request.
	idx  []int
	refs []ref.Ref
}

// Move implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) Move(ctx context.Context, req *connect.Request[chainmailv1.MoveRequest]) (*connect.Response[chainmailv1.MoveResponse], error) {
	folder := req.Msg.GetFolder()
	outs, err := o.batch(ctx, req.Header(), batchCall{
		action:  "move",
		refs:    req.Msg.GetRefs(),
		check:   func() error { return checkName("folder", folder) },
		targets: []string{folderTarget(folder)},
		run: func(ctx context.Context, w mailstore.Writer, refs []ref.Ref) []mailstore.RefResult {
			return w.Move(ctx, refs, folder)
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.MoveResponse{Outcomes: outs}), nil
}

// Copy implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) Copy(ctx context.Context, req *connect.Request[chainmailv1.CopyRequest]) (*connect.Response[chainmailv1.CopyResponse], error) {
	folder := req.Msg.GetFolder()
	outs, err := o.batch(ctx, req.Header(), batchCall{
		action:  "copy",
		refs:    req.Msg.GetRefs(),
		check:   func() error { return checkName("folder", folder) },
		targets: []string{folderTarget(folder)},
		run: func(ctx context.Context, w mailstore.Writer, refs []ref.Ref) []mailstore.RefResult {
			return w.Copy(ctx, refs, folder)
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.CopyResponse{Outcomes: outs}), nil
}

// Archive implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) Archive(ctx context.Context, req *connect.Request[chainmailv1.ArchiveRequest]) (*connect.Response[chainmailv1.ArchiveResponse], error) {
	outs, err := o.batch(ctx, req.Header(), batchCall{action: "archive", refs: req.Msg.GetRefs(), run: refsOnly(mailstore.Writer.Archive)})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.ArchiveResponse{Outcomes: outs}), nil
}

// Trash implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) Trash(ctx context.Context, req *connect.Request[chainmailv1.TrashRequest]) (*connect.Response[chainmailv1.TrashResponse], error) {
	outs, err := o.batch(ctx, req.Header(), batchCall{action: "trash", refs: req.Msg.GetRefs(), run: refsOnly(mailstore.Writer.Trash)})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.TrashResponse{Outcomes: outs}), nil
}

// Expunge implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) Expunge(ctx context.Context, req *connect.Request[chainmailv1.ExpungeRequest]) (*connect.Response[chainmailv1.ExpungeResponse], error) {
	outs, err := o.batch(ctx, req.Header(), batchCall{action: "expunge", refs: req.Msg.GetRefs(), run: refsOnly(mailstore.Writer.Expunge)})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.ExpungeResponse{Outcomes: outs}), nil
}

// SetFlags implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) SetFlags(ctx context.Context, req *connect.Request[chainmailv1.SetFlagsRequest]) (*connect.Response[chainmailv1.SetFlagsResponse], error) {
	var add, remove []string
	outs, err := o.batch(ctx, req.Header(), batchCall{
		action: "flags",
		refs:   req.Msg.GetRefs(),
		check: func() (err error) {
			if len(req.Msg.GetAdd()) == 0 && len(req.Msg.GetRemove()) == 0 {
				return &argError{msg: "no flags to add or remove", hint: "pass at least one flag to add or remove"}
			}
			if add, err = normalizeFlags(req.Msg.GetAdd()); err != nil {
				return err
			}
			remove, err = normalizeFlags(req.Msg.GetRemove())
			return err
		},
		run: func(ctx context.Context, w mailstore.Writer, refs []ref.Ref) []mailstore.RefResult {
			return w.StoreFlags(ctx, refs, add, remove)
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SetFlagsResponse{Outcomes: outs}), nil
}

// SetLabels implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) SetLabels(ctx context.Context, req *connect.Request[chainmailv1.SetLabelsRequest]) (*connect.Response[chainmailv1.SetLabelsResponse], error) {
	add, remove := req.Msg.GetAdd(), req.Msg.GetRemove()
	outs, err := o.batch(ctx, req.Header(), batchCall{
		action: "labels",
		refs:   req.Msg.GetRefs(),
		check: func() error {
			if len(add) == 0 && len(remove) == 0 {
				return &argError{msg: "no labels to add or remove", hint: "pass at least one label to add or remove"}
			}
			for _, l := range slices.Concat(add, remove) {
				if err := checkName("label", l); err != nil {
					return err
				}
			}
			return nil
		},
		run: func(ctx context.Context, w mailstore.Writer, refs []ref.Ref) []mailstore.RefResult {
			return w.StoreLabels(ctx, refs, add, remove)
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SetLabelsResponse{Outcomes: outs}), nil
}

// batch runs a batch call: it checks the call, runs each account's refs
// through one Writer call, at most maxAccountFanOut accounts at once, and
// records one audit row per account. A ref that fails, fails only its own
// outcome. A call refused for its arguments still records one row per
// account, unless it has too few or too many refs to attribute.
func (o *organizeService) batch(ctx context.Context, h http.Header, c batchCall) ([]*chainmailv1.RefOutcome, error) {
	who, err := callerOf(ctx, h)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, organizeTimeout)
	defer cancel()
	if err := checkRefCount(c); err != nil {
		entry := audit.Entry{Action: c.action, Targets: append([]string{tally("refs", len(c.refs))}, c.targets...)}
		return nil, o.s.record(ctx, who, entry, err)
	}
	outs, errs, groups := groupRefs(c)
	if c.check != nil {
		if err := c.check(); err != nil {
			for i := range errs {
				errs[i] = cmp.Or(errs[i], err)
			}
			if aerr := o.recordBatch(ctx, who, c, groups, errs); aerr != nil {
				return nil, aerr
			}
			return nil, toConnectError(err)
		}
	}
	o.runBatch(ctx, c, outs, errs, groups)
	if err := o.recordBatch(ctx, who, c, groups, errs); err != nil {
		return nil, err
	}
	return outs, nil
}

// checkRefCount refuses a call with no refs or too many refs.
func checkRefCount(c batchCall) error {
	if n := len(c.refs); n == 0 || n > maxOrganizeRefs {
		return &argError{
			msg:  fmt.Sprintf("%s takes 1 to %d refs, got %d", c.action, maxOrganizeRefs, n),
			hint: fmt.Sprintf("split the refs into batches of %d", maxOrganizeRefs),
		}
	}
	return nil
}

// groupRefs parses the call's refs and groups them by account. It returns
// an outcome per ref in request order, each ref's parse error, and the
// groups sorted by account, with refs that did not parse in a group of
// their own under account "".
func groupRefs(c batchCall) ([]*chainmailv1.RefOutcome, []error, []*refGroup) {
	outs := make([]*chainmailv1.RefOutcome, len(c.refs))
	errs := make([]error, len(c.refs))
	byAccount := map[string]*refGroup{}
	group := func(account string) *refGroup {
		g, ok := byAccount[account]
		if !ok {
			g = &refGroup{account: account}
			byAccount[account] = g
		}
		return g
	}
	for i, s := range c.refs {
		outs[i] = &chainmailv1.RefOutcome{Ref: s}
		r, err := ref.Parse(s)
		if err != nil {
			errs[i] = err
			g := group("")
			g.idx = append(g.idx, i)
			continue
		}
		g := group(r.Account)
		g.idx = append(g.idx, i)
		g.refs = append(g.refs, r)
	}
	groups := slices.Collect(maps.Values(byAccount))
	slices.SortFunc(groups, func(a, b *refGroup) int { return strings.Compare(a.account, b.account) })
	return outs, errs, groups
}

// runBatch runs the groups concurrently, one goroutine and one Writer call
// per account, at most maxAccountFanOut at once. Each store bounds its own
// folder groups by its pool size. It fills in outs and errs, and sets each
// failed outcome's error.
func (o *organizeService) runBatch(ctx context.Context, c batchCall, outs []*chainmailv1.RefOutcome, errs []error, groups []*refGroup) {
	sem := make(chan struct{}, maxAccountFanOut)
	var wg sync.WaitGroup
	for _, g := range groups {
		if g.account == "" {
			continue
		}
		acct, err := o.s.opts.Config.Account(g.account)
		if err != nil {
			for _, i := range g.idx {
				errs[i] = err
			}
			continue
		}
		w := o.s.stores.get(acct)
		// Taking the slot before starting the goroutine bounds the
		// goroutines as well as the Writer calls. Once the call is
		// canceled, no further account starts.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			for _, i := range g.idx {
				errs[i] = ctx.Err()
			}
			continue
		}
		// Each goroutine writes only its own group's positions.
		wg.Go(func() {
			defer func() { <-sem }()
			results := c.run(ctx, w, g.refs)
			for j, i := range g.idx {
				if j >= len(results) {
					errs[i] = errors.New("the store returned too few results")
					continue
				}
				res := results[j]
				if res.Err != nil {
					errs[i] = res.Err
					continue
				}
				if res.Ref != (ref.Ref{}) {
					if nr := res.Ref.String(); nr != c.refs[i] {
						outs[i].NewRef = nr
					}
				}
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			outs[i].Error = itemError(err)
		}
	}
}

// recordBatch writes one audit row per account: its refs, then the call's
// own targets, ok when every ref succeeded, else the first failure's
// reason. Refs that did not parse are recorded as invalidTarget. Every row
// is attempted, and the first failed write fails the call.
func (o *organizeService) recordBatch(ctx context.Context, who caller, c batchCall, groups []*refGroup, errs []error) error {
	var firstErr error
	for _, g := range groups {
		entry := audit.Entry{Action: c.action, Account: g.account, Result: audit.ResultOK}
		for _, i := range g.idx {
			target := invalidTarget
			if g.account != "" {
				target = c.refs[i]
			}
			entry.Targets = append(entry.Targets, target)
			if errs[i] != nil && entry.Result == audit.ResultOK {
				entry.Result, entry.Reason = audit.ResultError, apiReason(errs[i])
			}
		}
		entry.Targets = append(entry.Targets, c.targets...)
		if err := o.s.record(ctx, who, entry, nil); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CreateFolder implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) CreateFolder(ctx context.Context, req *connect.Request[chainmailv1.CreateFolderRequest]) (*connect.Response[chainmailv1.CreateFolderResponse], error) {
	name := req.Msg.GetName()
	var created string
	err := o.folderCall(ctx, req.Header(), "folders.create", req.Msg.GetAccount(), []string{name}, nil,
		func(ctx context.Context, w mailstore.Writer) (err error) {
			created, err = w.CreateFolder(ctx, name)
			return err
		})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.CreateFolderResponse{Name: created}), nil
}

// RenameFolder implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) RenameFolder(ctx context.Context, req *connect.Request[chainmailv1.RenameFolderRequest]) (*connect.Response[chainmailv1.RenameFolderResponse], error) {
	from, to := req.Msg.GetFrom(), req.Msg.GetTo()
	var renamed string
	err := o.folderCall(ctx, req.Header(), "folders.rename", req.Msg.GetAccount(), []string{from, to}, nil,
		func(ctx context.Context, w mailstore.Writer) (err error) {
			renamed, err = w.RenameFolder(ctx, from, to)
			return err
		})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.RenameFolderResponse{Name: renamed}), nil
}

// DeleteFolder implements chainmailv1connect.OrganizeServiceHandler.
func (o *organizeService) DeleteFolder(ctx context.Context, req *connect.Request[chainmailv1.DeleteFolderRequest]) (*connect.Response[chainmailv1.DeleteFolderResponse], error) {
	name, force := req.Msg.GetName(), req.Msg.GetForce()
	var extra []string
	if force {
		extra = []string{"force"}
	}
	err := o.folderCall(ctx, req.Header(), "folders.delete", req.Msg.GetAccount(), []string{name}, extra,
		func(ctx context.Context, w mailstore.Writer) error { return w.DeleteFolder(ctx, name, force) })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.DeleteFolderResponse{}), nil
}

// folderCall checks a folder call's account and folder names, runs it on
// the account's store, and records it with the names as targets, each
// valid one as given and each invalid one as invalidTarget, then extra. A
// store's error reaches the client with fixed text.
func (o *organizeService) folderCall(ctx context.Context, h http.Header, action, account string, names, extra []string, run func(context.Context, mailstore.Writer) error) error {
	who, err := callerOf(ctx, h)
	if err != nil {
		return err
	}
	ctx, cancel := withDeadline(ctx, organizeTimeout)
	defer cancel()
	entry := audit.Entry{Action: action}
	var callErr error
	entry.Account, callErr = accountTarget(account)
	for _, n := range names {
		target := n
		if err := checkName("folder", n); err != nil {
			target = invalidTarget
			callErr = cmp.Or(callErr, err)
		}
		entry.Targets = append(entry.Targets, target)
	}
	entry.Targets = append(entry.Targets, extra...)
	if callErr == nil {
		callErr = o.runFolderCall(ctx, account, run)
	}
	return o.s.record(ctx, who, entry, callErr)
}

// runFolderCall runs a checked folder call on account's store.
func (o *organizeService) runFolderCall(ctx context.Context, account string, run func(context.Context, mailstore.Writer) error) error {
	acct, err := o.s.opts.Config.Account(account)
	if err != nil {
		return messageErr(err)
	}
	return messageErr(run(ctx, o.s.stores.get(acct)))
}

// checkName refuses a folder or label name that no server has: empty,
// longer than maxFolderBytes, or holding a control byte. Proto strings are
// already UTF-8. The error leaves the name out.
func checkName(what, name string) error {
	ok := name != "" && len(name) <= maxFolderBytes &&
		!strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
	if ok {
		return nil
	}
	return &argError{
		msg:  fmt.Sprintf("the %s name is not valid", what),
		hint: fmt.Sprintf("%s names are 1 to %d bytes without control characters", what, maxFolderBytes),
	}
}

// folderTarget is the audit target for a destination folder.
func folderTarget(folder string) string {
	if checkName("folder", folder) != nil {
		return invalidTarget
	}
	return "folder=" + folder
}

// normalizeFlags checks flags a client asked to add or remove and returns
// them with system flags in their canonical case. The error leaves the
// flag out.
func normalizeFlags(flags []string) ([]string, error) {
	out := make([]string, len(flags))
	for i, f := range flags {
		if !strings.HasPrefix(f, `\`) {
			if !keywordRE.MatchString(f) {
				return nil, &argError{
					msg:  "a keyword is not valid",
					hint: "keywords are 1 to 64 letters, digits and $ _ . -",
				}
			}
			out[i] = f
			continue
		}
		j := slices.IndexFunc(storableFlags, func(s string) bool { return strings.EqualFold(s, f) })
		switch {
		case j >= 0:
			out[i] = storableFlags[j]
		case strings.EqualFold(f, `\Deleted`):
			return nil, &argError{msg: `\Deleted cannot be set or cleared`, hint: "to delete messages, use expunge"}
		default:
			return nil, &argError{msg: "a system flag cannot be set or cleared", hint: `the system flags are \Seen, \Flagged, \Answered and \Draft`}
		}
	}
	return out, nil
}

// refsOnly adapts a Writer method that takes nothing but refs, such as
// Archive, to batchCall.run.
func refsOnly(method func(mailstore.Writer, context.Context, []ref.Ref) []mailstore.RefResult) func(context.Context, mailstore.Writer, []ref.Ref) []mailstore.RefResult {
	return func(ctx context.Context, w mailstore.Writer, refs []ref.Ref) []mailstore.RefResult {
		return method(w, ctx, refs)
	}
}
