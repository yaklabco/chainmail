package imapstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Search implements mailstore.Store.
func (s *Store) Search(ctx context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	var base *imap.SearchCriteria
	if s.gmail() {
		base = gmailCriteria(compileGmail(req.Query, req.In))
	} else {
		var err error
		if base, err = compileStandard(req.Query, s.opts.Now()); err != nil {
			return nil, err
		}
	}
	names := slices.Sorted(maps.Keys(req.Bounds))
	if req.Bounds == nil {
		var err error
		if names, err = s.SearchFolders(ctx, req.In); err != nil {
			return nil, err
		}
	}
	results := make([]mailstore.SourceResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { results[i] = s.searchSource(ctx, name, base, req) })
	}
	wg.Wait()
	if err := accountError(ctx, results); err != nil {
		return nil, err
	}
	return results, nil
}

// accountError is the error that failed every source of a search at once: a
// login refusal, which is about the account and not one folder, or the end
// of ctx. A search with bounds meets these first in a source, since it
// skips the folder list.
func accountError(ctx context.Context, results []mailstore.SourceResult) error {
	errs := make([]error, len(results))
	for i, r := range results {
		errs[i] = r.Err
	}
	return accountFailure(ctx, errs)
}

// accountFailure picks, from per-folder errors, the one that is about the
// whole account: the end of ctx, or a login refusal. It returns nil when
// every error is about its own folder.
func accountFailure(ctx context.Context, errs []error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, err := range errs {
		if errors.Is(err, auth.ErrRejected) || errors.Is(err, auth.ErrAuthRequired) {
			return err
		}
	}
	return nil
}

func (s *Store) searchSource(ctx context.Context, folder string, base *imap.SearchCriteria, req mailstore.SearchRequest) mailstore.SourceResult {
	res := mailstore.SourceResult{Source: mailstore.Source{Folder: folder}}
	bound, continuing := req.Bounds[folder]
	err := s.withConn(ctx, folder, func(c *conn) error {
		res.UIDs, res.Count = nil, 0
		if err := s.open(ctx, c, folder); err != nil {
			return err
		}
		res.Source.UIDValidity = c.uidValidity
		if continuing && bound.UIDValidity != c.uidValidity {
			return fmt.Errorf("%w: %s changed since the first page; search again", mailstore.ErrStaleRef, folder)
		}
		criteria := *base
		criteria.UID = slices.Clone(base.UID)
		if continuing && bound.Below > 0 {
			if bound.Below == 1 {
				return nil // nothing is below UID 1
			}
			var below imap.UIDSet
			below.AddRange(1, imap.UID(bound.Below-1))
			criteria.UID = append(criteria.UID, below)
		}
		uids, count, err := c.search(ctx, &criteria, req.CountOnly)
		if err != nil {
			return err
		}
		res.UIDs, res.Count = uids, count
		return nil
	})
	res.Err = err
	return res
}

// search runs UID SEARCH and returns the UIDs, highest first, and their
// count. For counts it asks for ESEARCH COUNT where the server has it.
func (c *conn) search(ctx context.Context, criteria *imap.SearchCriteria, countOnly bool) ([]uint32, uint32, error) {
	var opts *imap.SearchOptions
	if countOnly && c.client().Caps().Has(imap.CapESearch) {
		opts = &imap.SearchOptions{ReturnCount: true}
	}
	var data *imap.SearchData
	if err := c.do(ctx, func() error {
		var err error
		data, err = c.client().UIDSearch(criteria, opts).Wait()
		return err
	}); err != nil {
		return nil, 0, err
	}
	if opts != nil {
		return nil, data.Count, nil
	}
	all := data.AllUIDs()
	uids := make([]uint32, len(all))
	for i, u := range all {
		uids[i] = uint32(u)
	}
	slices.SortFunc(uids, func(a, b uint32) int { return cmp.Compare(b, a) })
	count := uint32(len(uids))
	if countOnly {
		uids = nil
	}
	return uids, count, nil
}

// findMsgID returns the UID of the message with X-GM-MSGID id in the open folder, or 0.
func (c *conn) findMsgID(ctx context.Context, id uint64) (uint32, error) {
	uids, _, err := c.search(ctx, &imap.SearchCriteria{GmailMsgID: []uint64{id}}, false)
	if err != nil || len(uids) == 0 {
		return 0, err
	}
	return uids[0], nil
}

func (c *conn) fetch(ctx context.Context, set imap.UIDSet, opts *imap.FetchOptions) ([]*imapclient.FetchMessageBuffer, error) {
	var bufs []*imapclient.FetchMessageBuffer
	err := c.do(ctx, func() error {
		var err error
		bufs, err = c.client().Fetch(set, opts).Collect()
		return err
	})
	return bufs, err
}

// fetchEach runs one FETCH and passes each message of the answer to each as
// it arrives. Every message gives the command a fresh timeout, so the
// timeout bounds the silence between replies and not the whole answer: an
// answer for every message in a large folder can take longer than the
// timeout and still complete while the server keeps sending.
//
// each must not block or use c, and may see a truncated message just before
// an error; discard what it gathered when fetchEach fails.
func (c *conn) fetchEach(ctx context.Context, set imap.UIDSet, opts *imap.FetchOptions, each func(*imapclient.FetchMessageBuffer)) error {
	return c.pipeline(ctx, func(progress func()) error {
		cmd := c.client().Fetch(set, opts)
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			buf, err := msg.Collect()
			if err != nil {
				_ = cmd.Close() // drains the rest so the client reads on; the message's error is the one to report
				return err
			}
			progress()
			each(buf)
		}
		return cmd.Close()
	})
}

func uidSet(uids []uint32) imap.UIDSet {
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	return set
}

// summaryItems is what a row needs. fullHeader adds the whole header, for
// reading a message.
func (s *Store) summaryItems(preview, fullHeader bool) *imap.FetchOptions {
	o := &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true}, Preview: preview,
	}
	if s.gmail() {
		o.GmailMsgID, o.GmailThreadID, o.GmailLabels = true, true, true
	}
	switch {
	case fullHeader:
		o.BodySection = []*imap.FetchItemBodySection{headerSection}
	case !s.gmail():
		o.BodySection = []*imap.FetchItemBodySection{referencesSection}
	}
	return o
}

// FetchSummaries implements mailstore.Store.
func (s *Store) FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	var out []mailstore.Summary
	err := s.withConn(ctx, src.Folder, func(c *conn) error {
		out = nil
		if err := s.open(ctx, c, src.Folder); err != nil {
			return err
		}
		if c.uidValidity != src.UIDValidity {
			return fmt.Errorf("%w: %s changed; search again", mailstore.ErrStaleRef, src.Folder)
		}
		var err error
		out, err = s.fetchSummaries(ctx, c, src.Folder, uids, opts.Snippets)
		return err
	})
	return out, err
}

// fetchSummaries fetches summaries of uids in folder, which must be open on
// c, and returns them in the order of uids, leaving out any the server did
// not answer for. With snippets each gets PREVIEW when the server offers it
// and a text sample otherwise. When fetching the samples fails it returns
// the summaries with the error.
func (s *Store) fetchSummaries(ctx context.Context, c *conn, folder string, uids []uint32, snippets bool) ([]mailstore.Summary, error) {
	preview := snippets && c.client().Caps().Has(imap.CapPreview)
	bufs, err := c.fetch(ctx, uidSet(uids), s.summaryItems(preview, false))
	if err != nil {
		return nil, err
	}
	byUID := make(map[uint32]*imapclient.FetchMessageBuffer, len(bufs))
	for _, b := range bufs {
		byUID[uint32(b.UID)] = b
	}
	var out []mailstore.Summary
	var structures []imap.BodyStructure
	for _, uid := range uids {
		if b, ok := byUID[uid]; ok {
			out = append(out, s.summary(folder, c.uidValidity, b))
			structures = append(structures, b.BodyStructure)
		}
	}
	if snippets && !preview {
		return out, c.samples(ctx, out, structures)
	}
	return out, nil
}

// samples fills Sample with the first bytes of each message's text part.
// The FETCH commands are pipelined and read concurrently.
func (c *conn) samples(ctx context.Context, sums []mailstore.Summary, structures []imap.BodyStructure) error {
	type pending struct {
		i       int
		part    *imap.BodyStructureSinglePart
		section *imap.FetchItemBodySection
	}
	return c.pipeline(ctx, func(progress func()) error {
		var ps []pending
		col := newCollector(len(sums), progress)
		for i := range sums {
			path, part := textLeaf(structures[i])
			if part == nil {
				continue
			}
			sec := &imap.FetchItemBodySection{Part: path, Peek: true, Partial: &imap.SectionPartial{Size: mailstore.SampleBytes}}
			col.add(len(ps), c.client().Fetch(imap.UIDSetNum(imap.UID(sums[i].Ref.UID)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}))
			ps = append(ps, pending{i: i, part: part, section: sec})
		}
		replies, errs := col.wait()
		for k, p := range ps {
			var refusal *imap.Error
			switch err := errs[k]; {
			case errors.As(err, &refusal):
				continue // no snippet for this one
			case err != nil:
				return err
			}
			if len(replies[k]) == 0 {
				continue
			}
			sums[p.i].Sample = &mailstore.TextSample{
				MediaType: p.part.MediaType(), Charset: param(p.part.Params, "charset"),
				Encoding: p.part.Encoding, Data: replies[k][0].FindBodySection(p.section),
			}
		}
		return nil
	})
}

func (s *Store) summary(folder string, uidValidity uint32, b *imapclient.FetchMessageBuffer) mailstore.Summary {
	sum := mailstore.Summary{
		Ref:          ref.Ref{Account: s.acct.Name, Folder: folder, UIDValidity: uidValidity, UID: uint32(b.UID), GmailMsgID: b.GmailMsgID},
		InternalDate: b.InternalDate, Size: b.RFC822Size, Flags: flagStrings(b.Flags), Labels: b.GmailLabels, Preview: b.Preview,
	}
	sum.Date = sum.InternalDate
	if env := b.Envelope; env != nil {
		if !env.Date.IsZero() {
			sum.Date = env.Date
		}
		sum.From, sum.To, sum.Subject = addresses(env.From), addresses(env.To), env.Subject
	}
	sum.HasAttachment = hasAttachment(b.BodyStructure)
	if s.gmail() {
		if b.GmailThreadID != 0 {
			sum.ThreadID = strconv.FormatUint(b.GmailThreadID, 16)
		}
	} else {
		sum.ThreadID = mailstore.ThreadKey(rootOf(b))
	}
	return sum
}

// rootOf is the root Message-ID of b's thread, from References, In-Reply-To
// or its own Message-ID.
func rootOf(b *imapclient.FetchMessageBuffer) string {
	header := b.FindBodySection(referencesSection)
	if header == nil {
		header = b.FindBodySection(headerSection)
	}
	var inReplyTo []string
	var messageID string
	if b.Envelope != nil {
		inReplyTo, messageID = b.Envelope.InReplyTo, b.Envelope.MessageID
	}
	return mailstore.RootMessageID(msgIDList(header, "References"), inReplyTo, messageID)
}

func addresses(list []imap.Address) []mailstore.Address {
	var out []mailstore.Address
	for _, a := range list {
		if email := a.Addr(); email != "" {
			out = append(out, mailstore.Address{Name: a.Name, Email: email})
		}
	}
	return out
}

func flagStrings(flags []imap.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}
