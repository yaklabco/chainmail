package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
)

const (
	// maxUploads is how many files upload at once, one stream each.
	maxUploads = 4
	// uploadChunk is the size of each data part of an upload.
	uploadChunk = 256 << 10
	// maxInputBytes caps a body, an HTML file or a raw message read into
	// memory. It is the daemon's cap on a ComposeService request, above
	// every account's message size limit.
	maxInputBytes = 40 << 20
	// keyPrefix starts every idempotency key the CLI derives.
	keyPrefix = "cli-"
)

const sendHelp = `Send a new message from one account.

The body is Markdown, sent as a text part and an HTML part, unless --text
sends it as plain text. --body - reads it from stdin. --html-file sends an
HTML file as the body, with --body as its text part when given.

Each --attach file is attached. Each --inline NAME=F shows the image F
where the body refers to NAME, as in ![chart](chart.png) with
--inline chart.png=./chart.png.

Running the same send again within 7 days reports the first send instead
of sending twice: without --idempotency-key, the key is derived from the
account, recipients, subject, body and files. To send identical content
again on purpose, pass a new --idempotency-key.

--dry-run writes the message as MIME to stdout instead of sending it. It
refuses a terminal unless --output json, which prints it as a JSON string.

Examples:
  chainmail send --account work --to ann@example.com --subject "Q3" --body "See *attached*." --attach q3.pdf
  echo "Done." | chainmail send --account work --to ann@example.com --subject "Status" --body - --text
  chainmail send --account work --to ann@example.com --subject "Q3" --body-file q3.md --dry-run > q3.eml`

func newSendCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		m             messageFlags
		dryRun, draft bool
		key           string
	)
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a new message",
		Long:  sendHelp,
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun && draft {
				return usageError{err: errors.New("--dry-run and --draft cannot be used together"), hint: helpHint(cmd)}
			}
			if err := m.check(); err != nil {
				return err
			}
			jsonOut := explicitJSON(cmd, g)
			if dryRun {
				if err := checkDryRunOutput(env, jsonOut); err != nil {
					return err
				}
			}
			c := g.client(env)
			defer c.Close()
			spec, err := m.build(ctx, c, env.Stdin)
			if err != nil {
				return err
			}
			req := &chainmailv1.SendRequest{Spec: spec, DryRun: dryRun, Draft: draft, IdempotencyKey: key}
			if key == "" && !dryRun && !draft {
				req.IdempotencyKey = deriveKey(keyInput{
					account: spec.Account, to: spec.To, cc: spec.Cc, bcc: spec.Bcc,
					subject: spec.Subject, content: m.content.digest,
				})
			}
			resp, err := c.Compose.Send(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			return writeComposeResult(env, g, jsonOut, resp.Msg, resp.Msg.GetResult())
		},
	}
	addMessageFlags(cmd, &m, "the account to send from (required)")
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "write the message as MIME to stdout instead of sending it")
	f.BoolVar(&draft, "draft", false, "save the message to Drafts instead of sending it")
	f.StringVar(&key, "idempotency-key", "", "the key that stops a repeat from sending again (default: derived from the message)")
	return cmd
}

func newSendRawCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var account, file, key string
	cmd := &cobra.Command{
		Use:   "send-raw (--file F | -)",
		Short: "Send a complete RFC 5322 message as given",
		Long: `Send a complete RFC 5322 message as given, from --file or, with -, from stdin.
Its From must be the account's address and it must have a Message-ID.
The recipients are its To, Cc and Bcc; the Bcc header is removed before
sending. Without --idempotency-key, the key is derived from the account
and the bytes, so sending the same file again within 7 days does not send
twice. To send it again on purpose, pass a new --idempotency-key.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return usageError{err: err, hint: helpHint(cmd)}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			usage := func(msg string) error { return usageError{err: errors.New(msg), hint: helpHint(cmd)} }
			fromStdin := len(args) == 1
			switch {
			case fromStdin && args[0] != "-":
				return usage("the only argument send-raw takes is -, to read the message from stdin")
			case account == "":
				return usage("--account is required")
			case fromStdin && file != "":
				return usage("pass --file or -, not both")
			case !fromStdin && file == "":
				return usage("pass --file F, or - to read the message from stdin")
			}
			var (
				raw []byte
				err error
			)
			if fromStdin {
				raw, err = readInput(env.Stdin, "stdin")
			} else {
				raw, err = readFile(file, "--file")
			}
			if err != nil {
				return err
			}
			if len(raw) == 0 {
				return usage("the message is empty")
			}
			if key == "" {
				key = deriveKey(keyInput{account: account, raw: raw})
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Compose.SendRaw(ctx, connect.NewRequest(&chainmailv1.SendRawRequest{
				Account: account, Raw: raw, IdempotencyKey: key,
			}))
			if err != nil {
				return err
			}
			return writeComposeResult(env, g, explicitJSON(cmd, g), resp.Msg, resp.Msg.GetResult())
		},
	}
	f := cmd.Flags()
	f.StringVar(&account, "account", "", "the account to send from; the message's From must be its address (required)")
	f.StringVar(&file, "file", "", "the message to send")
	f.StringVar(&key, "idempotency-key", "", "the key that stops a repeat from sending again (default: derived from the bytes)")
	return cmd
}

// explicitJSON says --output json was given, rather than defaulted to
// because stdout is not a terminal.
func explicitJSON(cmd *cobra.Command, g *globals) bool {
	return cmd.Flags().Changed("output") && g.output == "json"
}

// checkDryRunOutput refuses to write a dry run's raw MIME to a terminal, as
// get --raw does, unless it goes out as a JSON string.
func checkDryRunOutput(env Env, jsonOut bool) error {
	if env.StdoutTerminal && !jsonOut {
		return usageError{
			err:  errors.New("refusing to write a raw message to a terminal"),
			hint: "redirect stdout, for example: > message.eml, or pass --output json",
		}
	}
	return nil
}

// dryRunJSON is a dry run printed as JSON, with the message as a string
// rather than protojson's base64.
type dryRunJSON struct {
	Result struct {
		DryRun struct {
			Raw          string   `json:"raw"`
			EnvelopeFrom string   `json:"envelope_from"`
			EnvelopeTo   []string `json:"envelope_to"`
		} `json:"dry_run"`
	} `json:"result"`
}

// writeComposeResult prints the result of a send-type call. A dry run is
// raw MIME, or a JSON string with jsonOut. Otherwise JSON output is resp,
// and text is a line or two about what was sent or saved.
func writeComposeResult(env Env, g *globals, jsonOut bool, resp proto.Message, res *chainmailv1.ComposeResult) error {
	if d := res.GetDryRun(); d != nil {
		if !jsonOut {
			if _, err := env.Stdout.Write(d.GetRaw()); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}
			return nil
		}
		var out dryRunJSON
		out.Result.DryRun.Raw = string(d.GetRaw())
		out.Result.DryRun.EnvelopeFrom = d.GetEnvelopeFrom()
		out.Result.DryRun.EnvelopeTo = append([]string{}, d.GetEnvelopeTo()...)
		return writeJSON(env.Stdout, out)
	}
	if !g.table() {
		return writeProto(env.Stdout, resp)
	}
	var b strings.Builder
	switch {
	case res.GetDraft() != nil:
		b.WriteString(draftSaved(res.GetDraft()))
	case res.GetSent() != nil:
		s := res.GetSent()
		fmt.Fprintf(&b, "sent %s\n", s.GetMessageId())
		switch {
		case s.GetSentRef() != "":
			fmt.Fprintf(&b, "saved to Sent: %s\n", s.GetSentRef())
		case s.GetSentCopyError() != "":
			b.WriteString(s.GetSentCopyError() + "\n")
		}
		if s.GetDuplicate() {
			b.WriteString("duplicate: already sent\n")
		}
	default:
		return errors.New("the daemon returned no result")
	}
	return writeString(env.Stdout, b.String())
}

// contentFlags are the body, attachment and option flags of every command
// that writes a message. The command sets account and stdin, then build
// reads the body, uploads the files and returns the Content.
type contentFlags struct {
	body, bodyFile, htmlFile string
	text, noSignature        bool
	attach, inline           []string

	// account is the account the uploads are for.
	account string
	// stdin is what --body - reads.
	stdin io.Reader

	cmd *cobra.Command
	// digest is set by build: the SHA-256 over everything that went into
	// the Content, with each file's bytes, for deriveKey.
	digest [32]byte
}

// addContentFlags registers f's flags on cmd.
func addContentFlags(cmd *cobra.Command, f *contentFlags) {
	f.cmd = cmd
	fl := cmd.Flags()
	fl.StringVar(&f.body, "body", "", "the body as Markdown, or - to read it from stdin")
	fl.StringVar(&f.bodyFile, "body-file", "", "read the Markdown body from this file")
	fl.BoolVar(&f.text, "text", false, "send the body as plain text instead of Markdown")
	fl.StringVar(&f.htmlFile, "html-file", "", "send this HTML file as the body, with --body as its text part when given")
	fl.StringArrayVar(&f.attach, "attach", nil, "attach this file (repeatable)")
	fl.StringArrayVar(&f.inline, "inline", nil, "show image file F inline where the body refers to NAME, as NAME=F (repeatable)")
	fl.BoolVar(&f.noSignature, "no-signature", false, "leave out the account's signature")
}

// hasBody says a body was given, as text or HTML.
func (f *contentFlags) hasBody() bool {
	return f.cmd.Flags().Changed("body") || f.bodyFile != "" || f.htmlFile != ""
}

func (f *contentFlags) usage(msg string) error {
	return usageError{err: errors.New(msg), hint: helpHint(f.cmd)}
}

// check validates the flags without reading anything.
func (f *contentFlags) check() error {
	hasText := f.cmd.Flags().Changed("body") || f.bodyFile != ""
	switch {
	case f.cmd.Flags().Changed("body") && f.bodyFile != "":
		return f.usage("--body and --body-file cannot be used together")
	case f.text && f.htmlFile != "":
		return f.usage("--text and --html-file cannot be used together")
	case f.text && !hasText:
		return f.usage("--text needs a body: --body or --body-file")
	}
	_, err := f.inlineFiles()
	return err
}

// inlineFiles parses the --inline flags.
func (f *contentFlags) inlineFiles() ([]uploadFile, error) {
	out := make([]uploadFile, 0, len(f.inline))
	for _, s := range f.inline {
		name, path, ok := strings.Cut(s, "=")
		if !ok || name == "" || path == "" {
			return nil, f.usage(fmt.Sprintf("--inline %q is not NAME=FILE", s))
		}
		out = append(out, uploadFile{name: name, path: path})
	}
	return out, nil
}

// build reads the body, uploads every --attach and --inline file
// concurrently, one stream each and at most maxUploads at once, and returns
// the Content. It sets f.digest.
func (f *contentFlags) build(ctx context.Context, c *client.Client) (*chainmailv1.Content, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	inline, err := f.inlineFiles()
	if err != nil {
		return nil, err
	}
	text, hasText, err := f.readText()
	if err != nil {
		return nil, err
	}
	var html []byte
	if f.htmlFile != "" {
		if html, err = readFile(f.htmlFile, "--html-file"); err != nil {
			return nil, err
		}
		if !utf8.Valid(html) {
			return nil, f.usage("--html-file is not UTF-8")
		}
	}
	files := make([]uploadFile, 0, len(f.attach)+len(inline))
	for _, p := range f.attach {
		files = append(files, uploadFile{path: p})
	}
	files = append(files, inline...)
	for _, file := range files {
		if err := checkReadable(file.path); err != nil {
			return nil, err
		}
	}
	ups, err := uploadAll(ctx, files, func(ctx context.Context, file uploadFile) (uploaded, error) {
		return uploadOne(ctx, c, f.account, file.path)
	})
	if err != nil {
		return nil, err
	}

	content := &chainmailv1.Content{NoSignature: f.noSignature}
	d := newKeyHash()
	switch {
	case f.htmlFile != "":
		content.Body = &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Html{Html: string(html)}, HtmlText: text}
		d.str("html", string(html))
		d.str("html_text", text)
	case f.text:
		content.Body = &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Text{Text: text}}
		d.str("text", text)
	case hasText:
		content.Body = &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Markdown{Markdown: text}}
		d.str("markdown", text)
	}
	if f.noSignature {
		d.str("no_signature", "")
	}
	for i, file := range files {
		u := ups[i]
		if file.name == "" {
			content.Attachments = append(content.Attachments, u.handle)
			d.str("attach", filepath.Base(file.path))
		} else {
			content.Inline = append(content.Inline, &chainmailv1.InlineImage{Name: file.name, Upload: u.handle})
			d.str("inline", file.name)
			d.str("inline_file", filepath.Base(file.path))
		}
		d.bytes("sha256", u.sum[:])
	}
	f.digest = d.sum()
	return content, nil
}

// readText reads the body text from --body, --body-file or stdin. ok is
// false when none was given.
func (f *contentFlags) readText() (text string, ok bool, err error) {
	var b []byte
	switch {
	case f.bodyFile != "":
		b, err = readFile(f.bodyFile, "--body-file")
	case !f.cmd.Flags().Changed("body"):
		return "", false, nil
	case f.body == "-":
		b, err = readInput(f.stdin, "stdin")
	default:
		return f.body, true, nil
	}
	if err != nil {
		return "", false, err
	}
	if !utf8.Valid(b) {
		return "", false, f.usage("the body is not UTF-8")
	}
	return string(b), true, nil
}

// readInput reads all of r, up to maxInputBytes.
func readInput(r io.Reader, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if len(b) > maxInputBytes {
		return nil, usageError{
			err:  fmt.Errorf("%s is over %d MiB, larger than any message the daemon takes", what, maxInputBytes>>20),
			hint: "send large files as links",
		}
	}
	return b, nil
}

// readFile reads the file a flag names. Failing to open it is a usage error.
func readFile(path, flag string) ([]byte, error) {
	if err := checkReadable(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, usageError{err: fmt.Errorf("%s: %w", flag, err), hint: "check the path"}
	}
	defer func() { _ = file.Close() }() // read-only: a close error loses nothing
	return readInput(file, path)
}

// checkReadable refuses a path that does not name a regular file.
func checkReadable(path string) error {
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return usageError{err: err, hint: "check the path"}
	case !st.Mode().IsRegular():
		return usageError{err: fmt.Errorf("%s is not a regular file", path), hint: "check the path"}
	}
	return nil
}

// uploadFile is a file to upload: an attachment, or an inline image when
// name, the body's reference to it, is set.
type uploadFile struct {
	name string
	path string
}

// uploaded is an upload's handle and the SHA-256 of the bytes sent.
type uploaded struct {
	handle string
	sum    [32]byte
}

// uploadAll runs up for every file, at most maxUploads at once, and returns
// the results in order. The first failure cancels the rest and is returned.
// Every upload has returned when uploadAll does.
func uploadAll(ctx context.Context, files []uploadFile, up func(context.Context, uploadFile) (uploaded, error)) ([]uploaded, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]uploaded, len(files))
	sem := make(chan struct{}, maxUploads)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i, file := range files {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			u, err := up(ctx, file)
			if err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = u
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// uploadOne streams the file at path to the daemon for account and returns
// its handle and the SHA-256 of the bytes sent. The media type comes from
// the extension, or else the daemon sniffs it.
func uploadOne(ctx context.Context, c *client.Client, account, path string) (uploaded, error) {
	file, err := os.Open(path)
	if err != nil {
		return uploaded{}, usageError{err: err, hint: "check the path"}
	}
	defer func() { _ = file.Close() }() // read-only: a close error loses nothing
	// Cancelling ends the stream without completing it, so a file that
	// fails to read is not stored in part.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := c.Compose.UploadAttachment(ctx)
	header := &chainmailv1.UploadHeader{
		Account:     account,
		Filename:    filepath.Base(path),
		ContentType: mime.TypeByExtension(filepath.Ext(path)),
	}
	sum := sha256.New()
	sendErr := stream.Send(&chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Header{Header: header}})
	buf := make([]byte, uploadChunk)
	for sendErr == nil {
		n, rerr := file.Read(buf)
		if n > 0 {
			sum.Write(buf[:n])
			sendErr = stream.Send(&chainmailv1.UploadAttachmentRequest{Part: &chainmailv1.UploadAttachmentRequest_Data{Data: buf[:n]}})
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			cancel()
			_, _ = stream.CloseAndReceive() // cancelled: the read error is the one to report
			return uploaded{}, fmt.Errorf("reading %s: %w", path, rerr)
		}
	}
	// A send that fails with io.EOF means the daemon answered early;
	// CloseAndReceive returns its answer.
	if sendErr != nil && !errors.Is(sendErr, io.EOF) {
		_, _ = stream.CloseAndReceive() // the send error is the one to report
		return uploaded{}, sendErr
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return uploaded{}, err
	}
	u := uploaded{handle: resp.Msg.GetHandle()}
	copy(u.sum[:], sum.Sum(nil))
	return u, nil
}

// keyInput is what a derived idempotency key covers (spec 6.3). Unused
// fields stay empty: send-raw has only account and raw, a reply has a ref.
type keyInput struct {
	account     string
	to, cc, bcc []string
	subject     string
	// content is contentFlags.digest.
	content [32]byte
	raw     []byte
	// ref is the message replied to or forwarded, or the draft sent.
	ref string
	// mode tells apart calls on the same ref: "reply", "reply-all",
	// "forward", "forward-no-attachments" or "draft-send".
	mode string
}

// deriveKey returns "cli-" and the unpadded base32 SHA-256 of in. Each
// recipient list is sorted, so the order of the flags does not matter.
func deriveKey(in keyInput) string {
	h := newKeyHash()
	h.str("account", in.account)
	for _, l := range []struct {
		name string
		list []string
	}{{"to", in.to}, {"cc", in.cc}, {"bcc", in.bcc}} {
		sorted := slices.Sorted(slices.Values(l.list))
		h.str(l.name, fmt.Sprint(len(sorted)))
		for _, a := range sorted {
			h.str(l.name, a)
		}
	}
	h.str("subject", in.subject)
	h.bytes("content", in.content[:])
	h.bytes("raw", in.raw)
	h.str("ref", in.ref)
	h.str("mode", in.mode)
	sum := h.sum()
	return keyPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
}

// keyHash hashes labelled, length-prefixed fields, so no two different
// inputs encode the same.
type keyHash struct{ h hash.Hash }

func newKeyHash() *keyHash { return &keyHash{h: sha256.New()} }

func (k *keyHash) bytes(label string, b []byte) {
	var n [8]byte
	for _, part := range [][]byte{[]byte(label), b} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		k.h.Write(n[:])
		k.h.Write(part)
	}
}

func (k *keyHash) str(label, s string) { k.bytes(label, []byte(s)) }

func (k *keyHash) sum() [32]byte {
	var out [32]byte
	copy(out[:], k.h.Sum(nil))
	return out
}
