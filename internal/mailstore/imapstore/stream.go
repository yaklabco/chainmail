package imapstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// chunkSize is how much of a part each FETCH asks for while streaming.
const chunkSize = 1 << 20

// StreamPart implements mailstore.Store. The returned reader holds a pooled
// connection until it is closed.
func (s *Store) StreamPart(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
	if r.Account != s.acct.Name {
		return mailstore.PartInfo{}, nil, fmt.Errorf("%w: the ref is for account %s", mailstore.ErrNotFound, r.Account)
	}
	var path []int
	if part != "" {
		p, ok := parsePart(part)
		if !ok {
			return mailstore.PartInfo{}, nil, fmt.Errorf("%w: no part %q", mailstore.ErrNotFound, part)
		}
		path = p
	}
	var info mailstore.PartInfo
	var pr *partReader
	err := s.retry(ctx, func() error {
		var err error
		info, pr, err = s.tryOpenPart(ctx, r, path)
		return err
	})
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	return info, pr, nil
}

// tryOpenPart borrows a connection and opens the part on it. The reader
// keeps the connection when that works; otherwise it goes straight back.
func (s *Store) tryOpenPart(ctx context.Context, r ref.Ref, path []int) (mailstore.PartInfo, *partReader, error) {
	c, err := s.borrow(ctx, r.Folder)
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	kept := false
	defer func() {
		if !kept {
			s.release(c)
		}
	}()
	info, pr, err := s.openPart(ctx, c, r, path)
	kept = err == nil
	return info, pr, err
}

// openPart locates the part and fetches its first chunk, so a failure before
// any byte reaches the caller can still be retried.
func (s *Store) openPart(ctx context.Context, c *conn, r ref.Ref, path []int) (mailstore.PartInfo, *partReader, error) {
	if err := s.open(ctx, c, r.Folder); err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	uid, err := s.resolveUID(ctx, c, r)
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	bufs, err := c.fetch(ctx, imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, RFC822Size: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true}})
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	if len(bufs) == 0 {
		return mailstore.PartInfo{}, nil, fmt.Errorf("%w: the message is gone; search again", mailstore.ErrStaleRef)
	}
	info := mailstore.PartInfo{ContentType: "message/rfc822", Size: bufs[0].RFC822Size}
	encoding := ""
	if path != nil {
		sp := findPart(bufs[0].BodyStructure, path)
		if sp == nil {
			return mailstore.PartInfo{}, nil, fmt.Errorf("%w: the message has no part %s", mailstore.ErrNotFound, partString(path))
		}
		info = mailstore.PartInfo{Filename: decodeWord(sp.Filename()), ContentType: sp.MediaType(), Size: int64(sp.Size)}
		encoding = sp.Encoding
	}
	pr := &partReader{s: s, c: c, ctx: ctx, uid: uid, path: path}
	if err := pr.fill(); err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	raw := io.Reader(rawReader{pr})
	switch strings.ToLower(encoding) {
	case "base64":
		pr.decoded = base64.NewDecoder(base64.StdEncoding, raw) // skips line breaks
	case "quoted-printable":
		pr.decoded = quotedprintable.NewReader(raw)
	default:
		pr.decoded = raw
	}
	return info, pr, nil
}

// partReader streams one part, fetching it in chunks with BODY.PEEK.
type partReader struct {
	s       *Store
	c       *conn
	ctx     context.Context // the request's context; it bounds every chunk fetch
	uid     uint32
	path    []int
	offset  int64
	buf     []byte
	eof     bool
	decoded io.Reader
	closed  bool
}

func (p *partReader) fill() error {
	sec := &imap.FetchItemBodySection{Part: p.path, Peek: true, Partial: &imap.SectionPartial{Offset: p.offset, Size: chunkSize}}
	bufs, err := p.c.fetch(p.ctx, imap.UIDSetNum(imap.UID(p.uid)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}})
	if err != nil {
		return err
	}
	var data []byte
	if len(bufs) > 0 {
		data = bufs[0].FindBodySection(sec)
	}
	p.offset += int64(len(data))
	p.buf = data
	p.eof = len(data) < chunkSize
	return nil
}

// Read implements io.Reader. A failure after the stream started is final
// and reported as unavailable.
func (p *partReader) Read(b []byte) (int, error) {
	n, err := p.decoded.Read(b)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, mailstore.ErrUnavailable) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: the stream broke after it started: %w", mailstore.ErrUnavailable, err)
	}
	return n, err
}

// Close returns the connection to the pool, or drops it if it broke.
func (p *partReader) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	p.s.release(p.c)
	return nil
}

// rawReader reads the still-encoded part bytes, fetching chunks as needed.
type rawReader struct{ p *partReader }

func (r rawReader) Read(b []byte) (int, error) {
	p := r.p
	for len(p.buf) == 0 {
		if p.eof {
			return 0, io.EOF
		}
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}
