package mcpserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
)

const (
	// maxNameBytes caps a saved file name, any " (n)" suffix included.
	maxNameBytes = 200
	// maxExtBytes is the longest extension clipping and numbering keep.
	maxExtBytes = 16
	// maxNameTries bounds the " (n)" suffixes tried for a free name.
	maxNameTries = 1000
	// maxTempTries bounds the random temporary names tried in a directory.
	maxTempTries = 10
	// uploadChunk is the size of each data part of an upload.
	uploadChunk = 256 << 10
)

// Hints for a destination the file tools refuse.
const (
	privateDirHint = "pick a directory this user owns that no other user can write to, or omit dir to use the default"
	hardLinkHint   = "pick a directory on a filesystem with hard links, such as the default (omit dir)"
)

// sanitizeName makes a message-derived file name safe to create: the base
// name only, without control or format characters (so no bidi overrides),
// leading dots or spaces (so never hidden, "." or ".."), trailing spaces, at
// most maxNameBytes. Empty becomes fallback.
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
	// One pass takes the whole leading run, so ". .bashrc" cannot leave ".bashrc".
	name = strings.TrimLeftFunc(name, func(r rune) bool { return r == '.' || unicode.IsSpace(r) })
	if len(name) > maxNameBytes {
		name = clipName(name)
	}
	name = strings.TrimRightFunc(name, unicode.IsSpace)
	if name == "" || name == "." || name == ".." {
		return fallback
	}
	return name
}

// clipName cuts name to maxNameBytes on a rune boundary, keeping a short extension.
func clipName(name string) string {
	ext := nameExt(name)
	return clipBytes(strings.TrimSuffix(name, ext), maxNameBytes-len(ext)) + ext
}

// numberedName is name with " (n)" before its extension, the stem clipped
// so the whole stays within maxNameBytes.
func numberedName(name string, n int) string {
	ext := nameExt(name)
	suffix := fmt.Sprintf(" (%d)", n)
	return clipBytes(strings.TrimSuffix(name, ext), maxNameBytes-len(suffix)-len(ext)) + suffix + ext
}

// nameExt is the extension clipping and numbering keep: none when it is long.
func nameExt(name string) string {
	if ext := filepath.Ext(name); len(ext) <= maxExtBytes {
		return ext
	}
	return ""
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// outputDir opens the directory a download goes to and returns it with the
// path to report. dir, when given, must be an absolute path to an existing
// directory this user owns that no other user can write to. The default is
// created 0700 when missing; one that exists must be a real directory (not a
// symlink) this user owns, and is made 0700 if group or others have access.
// The checks run on the opened directory, and saveFile writes through it, so
// a path swapped later cannot move the file. The caller closes the root.
func (s *Server) outputDir(dir string) (*os.Root, string, error) {
	if dir == "" {
		return openDefaultDir(s.opts.AttachmentsDir)
	}
	if !filepath.IsAbs(dir) {
		return nil, "", invalidArg("dir must be an absolute path, got %q", dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, "", invalidArg("dir %s %s", dir, fsProblem(err))
	}
	pre, err := os.Stat(resolved)
	switch {
	case err != nil:
		return nil, "", invalidArg("dir %s %s", dir, fsProblem(err))
	case !pre.IsDir():
		return nil, "", invalidArg("dir %s is not a directory", dir)
	}
	root, st, err := openDir(resolved)
	if err != nil {
		return nil, "", invalidArg("dir %s %s", dir, fsProblem(err))
	}
	var problem string
	switch {
	case !os.SameFile(pre, st):
		problem = "changed while it was opened"
	case !ownedByMe(st):
		problem = "is not owned by this user"
	case st.Mode().Perm()&0o022 != 0:
		problem = fmt.Sprintf("can be written by other users (mode %04o)", st.Mode().Perm())
	}
	if problem != "" {
		_ = root.Close() // opened read-only: a close error loses nothing
		return nil, "", hinted(api.ReasonInvalidArgument, privateDirHint, "dir %s %s", dir, problem)
	}
	return root, resolved, nil
}

// openDefaultDir creates or checks the default directory, then opens it.
func openDefaultDir(def string) (*os.Root, string, error) {
	if err := os.MkdirAll(def, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating %s: %w", def, err)
	}
	pre, err := os.Lstat(def)
	if err != nil {
		return nil, "", fmt.Errorf("checking %s: %w", def, err)
	}
	hint := defaultDirHint
	if !pre.IsDir() { // Lstat: a symlink is not a directory here
		return nil, "", hinted(api.ReasonInternal, hint, "the default attachments directory is a symlink or not a directory")
	}
	root, st, err := openDir(def)
	if err != nil {
		return nil, "", fmt.Errorf("opening %s: %w", def, err)
	}
	var problem error
	switch {
	case !os.SameFile(pre, st):
		problem = hinted(api.ReasonInternal, hint, "the default attachments directory changed while it was opened")
	case !ownedByMe(st):
		problem = hinted(api.ReasonInternal, hint, "the default attachments directory is not owned by this user")
	case st.Mode().Perm() != 0o700:
		if err := root.Chmod(".", 0o700); err != nil {
			problem = fmt.Errorf("making %s private: %w", def, err)
		}
	}
	if problem != nil {
		_ = root.Close() // opened read-only: a close error loses nothing
		return nil, "", problem
	}
	return root, def, nil
}

// openDir opens path as a root and stats what was opened.
func openDir(path string) (*os.Root, fs.FileInfo, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := root.Stat(".")
	if err != nil {
		_ = root.Close() // opened read-only: a close error loses nothing
		return nil, nil, err
	}
	return root, st, nil
}

// hinted is a tool error with a reason and a hint.
func hinted(reason, hint, format string, args ...any) error {
	return &toolError{body: errorBody{Reason: reason, Message: fmt.Sprintf(format, args...), Hint: hint}}
}

// saveFile writes what write produces to a new temporary file in root (mode
// 0600, a random name, never an existing file), syncs it, then links it to
// name or the first free "name (n).ext". dir is root's path, for the result
// and messages. It never replaces a file, and leaves nothing behind on failure.
func saveFile(root *os.Root, dir, name string, write func(io.Writer) (int64, error)) (string, int64, error) {
	tmpName, tmp, err := createTemp(root)
	if err != nil {
		return "", 0, fmt.Errorf("creating a file in %s: %w", dir, err)
	}
	n, werr := write(tmp)
	if werr == nil {
		werr = tmp.Sync()
	}
	if err := errors.Join(werr, tmp.Close()); err != nil {
		_ = root.Remove(tmpName) // best effort; the write error is the one to report
		return "", 0, err
	}
	final, err := linkUnique(root, root.Link, tmpName, dir, name)
	if err != nil {
		return "", 0, err
	}
	return filepath.Join(dir, final), n, nil
}

// createTemp creates a file under a fresh random name in root, failing
// rather than open an existing file.
func createTemp(root *os.Root) (string, *os.File, error) {
	for range maxTempTries {
		name := ".chainmail-" + rand.Text() + ".part"
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return name, f, err
	}
	return "", nil, fmt.Errorf("no free temporary name after %d tries", maxTempTries)
}

// linkUnique gives tmp a final name in root through link (root.Link outside
// tests), which fails rather than replace an existing file, then removes tmp.
// It returns the name used. A filesystem without hard links is refused
// with a hint: renaming instead could replace a file.
func linkUnique(root *os.Root, link func(oldname, newname string) error, tmp, dir, name string) (string, error) {
	defer func() { _ = root.Remove(tmp) }() // the link holds the data; tmp is only a name
	for n := 1; n <= maxNameTries; n++ {
		candidate := name
		if n > 1 {
			candidate = numberedName(name, n)
		}
		err := link(tmp, candidate)
		switch {
		case err == nil:
			return candidate, nil
		case errors.Is(err, fs.ErrExist):
			continue
		case linkUnsupported(err):
			return "", hinted(api.ReasonInvalidArgument, hardLinkHint, "cannot save here: the directory's filesystem does not support hard links")
		default:
			return "", fmt.Errorf("saving %s: %w", filepath.Join(dir, candidate), err)
		}
	}
	return "", fmt.Errorf("no free name for %s in %s after %d tries", name, dir, maxNameTries)
}

// linkUnsupported reports whether a link error means the filesystem has no hard links.
func linkUnsupported(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EXDEV, syscall.EMLINK} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// uploadedFile is one attachment streamed to the daemon.
type uploadedFile struct {
	Handle string // pass in Content.attachments or InlineImage.upload
	SHA256 string // lowercase hex of the bytes, as the daemon reports it
}

// attachmentFile opens a compose attachment path, following symlinks, and
// checks that what it opened is a regular file. O_NONBLOCK keeps a FIFO
// from blocking the open; regular files read the same with it.
func attachmentFile(p string) (*os.File, error) {
	if !filepath.IsAbs(p) {
		return nil, invalidArg("attachment %q is not an absolute path", p)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, invalidArg("attachment %s %s", p, fsProblem(err))
	}
	st, err := f.Stat()
	switch {
	case err != nil:
		_ = f.Close() // read-only: a close error loses nothing
		return nil, invalidArg("attachment %s %s", p, fsProblem(err))
	case !st.Mode().IsRegular():
		_ = f.Close() // as above
		return nil, invalidArg("attachment %s is not a regular file", p)
	}
	return f, nil
}

// uploadAttachments opens and checks every path first (absolute, symlinks
// followed, a regular file), then streams each opened file to the daemon
// for account, in order, so a path changed after the check changes nothing.
// Errors are *toolError values (INVALID_ARGUMENT for a bad path).
// Nil or empty paths return nil, nil without a daemon call.
func (s *Server) uploadAttachments(ctx context.Context, account string, paths []string) ([]uploadedFile, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	files := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range files {
			_ = f.Close() // read-only: a close error loses nothing
		}
	}()
	for _, p := range paths {
		f, err := attachmentFile(p)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	out := make([]uploadedFile, 0, len(paths))
	for i, f := range files {
		u, err := s.uploadOne(ctx, account, f, filepath.Base(paths[i]))
		if err != nil {
			return nil, s.errorResult(err)
		}
		out = append(out, u)
	}
	return out, nil
}

// uploadOne streams file to the daemon under filename. The media type comes
// from the extension, or else the daemon sniffs it.
func (s *Server) uploadOne(ctx context.Context, account string, file *os.File, filename string) (uploadedFile, error) {
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
			return uploadedFile{}, fmt.Errorf("reading %s: %w", file.Name(), rerr)
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

// defaultDirHint is how to repair the default attachments directory. It is
// fixed text: the path comes from the environment, not from the agent.
const defaultDirHint = "remove or fix the default attachments directory ($XDG_CACHE_HOME/chainmail/attachments, else ~/.cache/chainmail/attachments), or pass dir"

// fsProblem explains a filesystem error in fixed words. The error's own
// text can carry a resolved path or a name taken from mail, and file errors
// reach the agent as trusted text, so it is never quoted.
func fsProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	case errors.Is(err, fs.ErrPermission):
		return "is not accessible to this user"
	default:
		return "cannot be opened"
	}
}
