package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

const (
	// maxNameBytes caps a file name taken from a message, before any " (n)" suffix.
	maxNameBytes = 200
	// maxExtBytes is the longest extension clipping keeps.
	maxExtBytes = 16
	// maxNameTries bounds the " (n)" suffixes tried for a free name.
	maxNameTries = 1000
	// uploadChunk is the size of each data part of an upload.
	uploadChunk = 256 << 10
)

// sanitizeName makes a message-derived file name safe to create: the base
// name only, without control or format characters (so no bidi overrides),
// leading dots or surrounding space, at most maxNameBytes. Empty becomes fallback.
func sanitizeName(name, fallback string) string {
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(name), "."))
	if len(name) > maxNameBytes {
		name = clipName(name)
	}
	if name == "" {
		return fallback
	}
	return name
}

// clipName cuts name to maxNameBytes on a rune boundary, keeping a short extension.
func clipName(name string) string {
	ext := filepath.Ext(name)
	if len(ext) > maxExtBytes {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for len(stem)+len(ext) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	return stem + ext
}

// outputDir is where a download goes: dir, which must be an absolute path
// to an existing directory, or the default, created 0700 when missing.
func (s *Server) outputDir(dir string) (string, error) {
	if dir == "" {
		if err := os.MkdirAll(s.opts.AttachmentsDir, 0o700); err != nil {
			return "", fmt.Errorf("creating %s: %w", s.opts.AttachmentsDir, err)
		}
		return s.opts.AttachmentsDir, nil
	}
	if !filepath.IsAbs(dir) {
		return "", invalidArg("dir must be an absolute path, got %q", dir)
	}
	st, err := os.Stat(dir)
	switch {
	case err != nil:
		return "", invalidArg("dir %s: %v", dir, err)
	case !st.IsDir():
		return "", invalidArg("dir %s is not a directory", dir)
	}
	return dir, nil
}

// saveFile writes what write produces to a temporary file in dir (mode
// 0600), syncs it, then links it to name or the first free "name (n).ext".
// It never replaces a file, and leaves nothing behind on failure.
func saveFile(dir, name string, write func(io.Writer) (int64, error)) (string, int64, error) {
	tmp, err := os.CreateTemp(dir, ".chainmail-*.part")
	if err != nil {
		return "", 0, fmt.Errorf("creating a file in %s: %w", dir, err)
	}
	n, werr := write(tmp)
	if werr == nil {
		werr = tmp.Sync()
	}
	if err := errors.Join(werr, tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name()) // best effort; the write error is the one to report
		return "", 0, err
	}
	path, err := linkUnique(tmp.Name(), dir, name)
	if err != nil {
		return "", 0, err
	}
	return path, n, nil
}

// linkUnique gives tmp a final name in dir by hard link, which fails rather
// than replace an existing file, then removes tmp.
func linkUnique(tmp, dir, name string) (string, error) {
	defer func() { _ = os.Remove(tmp) }() // the link holds the data; tmp is only a name
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 1; n <= maxNameTries; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		path := filepath.Join(dir, candidate)
		err := os.Link(tmp, path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("saving %s: %w", path, err)
		}
	}
	return "", fmt.Errorf("no free name for %s in %s after %d tries", name, dir, maxNameTries)
}

// uploadedFile is one attachment streamed to the daemon.
type uploadedFile struct {
	Handle string // pass in Content.attachments or InlineImage.upload
	SHA256 string // lowercase hex of the bytes, as the daemon reports it
}

// attachmentFile checks a compose attachment path and returns it with symlinks resolved.
func attachmentFile(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", invalidArg("attachment %q is not an absolute path", p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", invalidArg("attachment %s: %v", p, err)
	}
	st, err := os.Stat(resolved)
	switch {
	case err != nil:
		return "", invalidArg("attachment %s: %v", p, err)
	case !st.Mode().IsRegular():
		return "", invalidArg("attachment %s is not a regular file", p)
	}
	return resolved, nil
}

// uploadAttachments checks every path first (absolute, symlinks resolved,
// a regular file), then streams each file to the daemon for account, in
// order. Errors are *toolError values (INVALID_ARGUMENT for a bad path).
// Nil or empty paths return nil, nil without a daemon call.
func (s *Server) uploadAttachments(ctx context.Context, account string, paths []string) ([]uploadedFile, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	resolved := make([]string, len(paths))
	for i, p := range paths {
		r, err := attachmentFile(p)
		if err != nil {
			return nil, err
		}
		resolved[i] = r
	}
	out := make([]uploadedFile, 0, len(paths))
	for i, p := range resolved {
		u, err := s.uploadOne(ctx, account, p, filepath.Base(paths[i]))
		if err != nil {
			return nil, s.errorResult(err)
		}
		out = append(out, u)
	}
	return out, nil
}

// uploadOne streams the file at path to the daemon under filename. The media
// type comes from the extension, or else the daemon sniffs it.
func (s *Server) uploadOne(ctx context.Context, account, path, filename string) (uploadedFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return uploadedFile{}, invalidArg("attachment %s: %v", path, err)
	}
	defer func() { _ = file.Close() }() // read-only: a close error loses nothing
	// Cancelling ends the stream without completing it, so a file that fails
	// to read is not stored in part.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := s.daemon.Compose.UploadAttachment(ctx)
	header := &chainmailv1.UploadHeader{Account: account, Filename: filename, ContentType: mime.TypeByExtension(filepath.Ext(filename))}
	sendErr := stream.Send(&chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Header{Header: header}})
	buf := make([]byte, uploadChunk)
	for sendErr == nil {
		n, rerr := file.Read(buf)
		if n > 0 {
			sendErr = stream.Send(&chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Data{Data: buf[:n]}})
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			cancel()
			_, _ = stream.CloseAndReceive() // cancelled: the read error is the one to report
			return uploadedFile{}, fmt.Errorf("reading %s: %w", path, rerr)
		}
	}
	// A send failing with io.EOF means the daemon answered early; CloseAndReceive has its answer.
	if sendErr != nil && !errors.Is(sendErr, io.EOF) {
		_, _ = stream.CloseAndReceive() // the send error is the one to report
		return uploadedFile{}, sendErr
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return uploadedFile{}, err
	}
	return uploadedFile{Handle: resp.Msg.GetHandle(), SHA256: resp.Msg.GetSha256()}, nil
}
