package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
)

const searchHelp = `Search mail with Gmail-style operators. Terms are joined by AND, -term
negates, OR joins (it binds tighter than AND, as in Gmail), parentheses
group, and values with spaces are quoted.

  word, "a phrase"              text anywhere in the message
  from: to: cc: bcc:            address or display name contains
  subject:                      subject contains
  is:unread is:read is:starred
  newer_than: older_than:       days, weeks, months or years: 7d, 2w, 3m, 1y
  after: before:                YYYY-MM-DD
  larger: smaller:              bytes, or KiB and MiB: 10K, 5M
  has:attachment filename: label:
                                Google accounts, and other accounts once
                                their cache is complete (see chainmail
                                cache status); label: there matches IMAP
                                keywords
  in:                           inbox, sent, drafts, archive, trash, spam
                                (or junk), anywhere, or a folder name
  account:                      this account; each account: term and each
                                --account adds one

Every folder except Trash and Spam is searched unless in: says otherwise.
On accounts other than Google, dates work at day granularity on the arrival
date, so newer_than:1d there matches since the start of yesterday.

Each account is answered from the local cache when the cache can answer
the whole query and is fresh, and by its mail server otherwise. The table
output ends with a source line naming which answered each account.
--source cache answers from the cache even when it is incomplete or stale,
and says why; --source server skips the cache.

The shell removes quotes, so quote the whole query when it holds a phrase
or a quoted value, as in the second example below. Put -- before the query
when a term starts with -, so it is not read as a flag:
  chainmail search -- invoice -from:acme`

func newFoldersCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var accounts []string
	cmd := &cobra.Command{
		Use:   "folders",
		Short: "List folders and their message counts",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Message.ListFolders(ctx, connect.NewRequest(&chainmailv1.ListFoldersRequest{Accounts: accounts}))
			if err != nil {
				return err
			}
			if g.table() {
				return foldersTable(env.Stdout, resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
	cmd.Flags().StringArrayVar(&accounts, "account", nil, "only this account (repeatable)")
	return cmd
}

// searchSources are the values --source takes.
var searchSources = []string{"auto", "cache", "server"}

func newSearchCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	req := &chainmailv1.SearchRequest{}
	cmd := &cobra.Command{
		Use:   "search [query...]",
		Short: "Search mail on every account, newest first",
		Long:  searchHelp,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("source") && !slices.Contains(searchSources, req.GetSource()) {
				return usageError{err: fmt.Errorf("--source must be auto, cache or server, got %q", req.GetSource()), hint: helpHint(cmd)}
			}
			req.QueryString = strings.Join(args, " ")
			c := g.client(env)
			defer c.Close()
			resp, err := c.Message.Search(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			switch {
			case !g.table():
				return writeProto(env.Stdout, resp.Msg)
			case req.GetCountOnly():
				return countTable(env.Stdout, resp.Msg)
			default:
				return searchTable(env.Stdout, env.location(), resp.Msg)
			}
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&req.Accounts, "account", nil, "only this account (repeatable)")
	f.Uint32Var(&req.Limit, "limit", 0, "rows per page, at most 500 (default 50)")
	f.StringVar(&req.Cursor, "cursor", "", "continue from the previous page's cursor")
	f.BoolVar(&req.CountOnly, "count", false, "count matches instead of listing them")
	f.BoolVar(&req.Snippets, "snippet", false, "add a snippet of up to 200 characters to each row")
	f.BoolVar(&req.Threads, "threads", false, "show the newest message of each thread")
	f.StringVar(&req.Source, "source", "", "where answers come from: auto, cache or server (default auto: the cache when it is complete and fresh)")
	return cmd
}

func newGetCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var full, raw bool
	cmd := &cobra.Command{
		Use:   "get <ref>...",
		Short: "Read up to 50 messages",
		Args:  minArgs(1),
		RunE: func(cmd *cobra.Command, refs []string) error {
			if raw {
				switch {
				case len(refs) != 1:
					return usageError{err: errors.New("--raw takes exactly one ref"), hint: helpHint(cmd)}
				case full:
					return usageError{err: errors.New("--raw and --full cannot be used together"), hint: helpHint(cmd)}
				case env.StdoutTerminal:
					// Raw bytes skip the daemon's text cleaning, so a sender's
					// escape sequences would reach the terminal.
					return usageError{err: errors.New("refusing to write a raw message to a terminal"), hint: "redirect stdout, for example: > message.eml"}
				}
			}
			c := g.client(env)
			defer c.Close()
			if raw {
				return writeRaw(ctx, c, env, refs[0])
			}
			resp, err := c.Message.Get(ctx, connect.NewRequest(&chainmailv1.GetRequest{Refs: refs, Full: full}))
			if err != nil {
				return err
			}
			if g.table() {
				err = messagesText(env.Stdout, env.location(), resp.Msg.GetItems())
			} else {
				err = writeProto(env.Stdout, resp.Msg)
			}
			if err != nil {
				return err
			}
			return itemsResult(resp.Msg.GetItems())
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "keep quoted text and signatures, and do not cut at body_limit")
	cmd.Flags().BoolVar(&raw, "raw", false, "write the original RFC 5322 bytes of one message to stdout, which must not be a terminal")
	return cmd
}

// itemsResult fails get only when every item failed, with the first one's reason.
func itemsResult(items []*chainmailv1.GetItem) error {
	for _, it := range items {
		if it.GetMessage() != nil {
			return nil
		}
	}
	if len(items) == 0 {
		return nil
	}
	e := items[0].GetError()
	reason := e.GetReason()
	if reason == "" {
		reason = api.ReasonInternal
	}
	return &exitError{reason: reason, msg: e.GetMessage(), hint: e.GetHint()}
}

func writeRaw(ctx context.Context, c *client.Client, env Env, ref string) error {
	stream, err := c.Message.GetRaw(ctx, connect.NewRequest(&chainmailv1.GetRawRequest{Ref: ref}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }() // the stream is drained or failed by then; Err reports the outcome
	for stream.Receive() {
		if _, err := env.Stdout.Write(stream.Msg().GetData()); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
	}
	return stream.Err()
}

func newThreadCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "thread <ref>",
		Short: "Read a message's whole thread, oldest first",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Message.GetThread(ctx, connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: args[0], Full: full}))
			if err != nil {
				return err
			}
			if g.table() {
				return threadText(env.Stdout, env.location(), resp.Msg, full)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "keep quoted text and signatures, and do not cut at body_limit")
	return cmd
}

func newAttachmentCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "attachment <ref> <part>",
		Short: "Download one attachment, by the part number get lists",
		Args:  exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case out == "":
				return usageError{err: errors.New("--out is required: a file path, or - for stdout"), hint: helpHint(cmd)}
			case out == "-" && env.StdoutTerminal:
				return usageError{err: errors.New("refusing to write an attachment to a terminal"), hint: "pass --out <path>, or pipe stdout"}
			}
			c := g.client(env)
			defer c.Close()
			stream, err := c.Message.DownloadAttachment(ctx, connect.NewRequest(&chainmailv1.DownloadAttachmentRequest{Ref: args[0], Part: args[1]}))
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }() // as in writeRaw
			if !stream.Receive() {
				if err := stream.Err(); err != nil {
					return err
				}
				return errors.New("the daemon sent no attachment")
			}
			header := stream.Msg().GetHeader()
			if header == nil {
				return errors.New("the daemon's first message was not the attachment header")
			}
			w, finish, err := openOutput(env, out)
			if err != nil {
				return err
			}
			var n int64
			for stream.Receive() {
				data := stream.Msg().GetData()
				if _, err := w.Write(data); err != nil {
					return finish(fmt.Errorf("writing the attachment: %w", err))
				}
				n += int64(len(data))
			}
			if err := finish(stream.Err()); err != nil {
				return err
			}
			if out == "-" {
				return nil
			}
			if g.table() {
				return writeString(env.Stdout, fmt.Sprintf("saved %d bytes to %s\n", n, out))
			}
			return writeJSON(env.Stdout, map[string]any{
				"path": out, "bytes": n, "content_type": header.GetContentType(),
				"filename": map[string]string{"untrusted": header.GetFilename().GetUntrusted()},
			})
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write, or - for stdout when it is not a terminal (required)")
	return cmd
}

// openOutput returns where to write a download. A file is written under a
// temporary name and renamed into place by finish, so a failed download
// leaves nothing behind. finish takes the copy's error and returns the
// overall one.
func openOutput(env Env, path string) (io.Writer, func(error) error, error) {
	if path == "-" {
		return env.Stdout, func(err error) error { return err }, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".chainmail-download-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating %s: %w", path, err)
	}
	finish := func(copyErr error) error {
		if copyErr == nil {
			copyErr = tmp.Sync() // so a crash cannot leave an empty file under the final name
		}
		if err := errors.Join(copyErr, tmp.Close()); err != nil {
			_ = os.Remove(tmp.Name()) // best effort; the copy error is the one to report
			return err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			_ = os.Remove(tmp.Name()) // as above
			return fmt.Errorf("saving %s: %w", path, err)
		}
		return nil
	}
	return tmp, finish, nil
}
