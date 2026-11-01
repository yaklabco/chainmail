package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

// registerFileTools adds download_attachment and save_raw_message. Both only
// read mail, write a new local file each call and never replace one.
func (s *Server) registerFileTools() {
	// The spec table: read only, not destructive, idempotent, open world.
	fileHints := hints{readOnly: true, destructive: false, idempotent: true, openWorld: true}
	addTool(s, "download_attachment", "Download an attachment",
		"Save one attachment to a file and return its path. The file name comes from the message: it is sanitized, it is untrusted, as are the path ending in it and the content type, and an existing file is never replaced (a clash gets a (2), (3) suffix). dir, when given, must be an absolute path to an existing directory this user owns that no other user can write to.",
		fileHints, s.downloadAttachment)
	addTool(s, "save_raw_message", "Save the raw message",
		"Save the original RFC 5322 bytes of a message to a .eml file and return its path. An existing file is never replaced. dir, when given, must be an absolute path to an existing directory this user owns that no other user can write to.",
		fileHints, s.saveRawMessage)
}

type downloadIn struct {
	Ref  string `json:"ref" jsonschema:"the message, from search or get_message"`
	Part string `json:"part" jsonschema:"an attachment part from get_message, such as 2 or 1.2"`
	Dir  string `json:"dir,omitempty" jsonschema:"an existing directory as an absolute path, owned by this user and writable by no other user; default ~/.cache/chainmail/attachments"`
}

type saveRawIn struct {
	Ref string `json:"ref" jsonschema:"the message, from search or get_message"`
	Dir string `json:"dir,omitempty" jsonschema:"an existing directory as an absolute path, owned by this user and writable by no other user; default ~/.cache/chainmail/attachments"`
}

// savedFileOut is a saved raw message. Its path is the directory the agent
// named, or the default, and a fixed file name, so nothing in it is untrusted.
type savedFileOut struct {
	Path        string `json:"path" jsonschema:"the absolute path of the new file"`
	Size        int64  `json:"size" jsonschema:"bytes written"`
	ContentType string `json:"content_type"`
}

// attachmentFileOut is a saved attachment. The file name and content type
// come from the message, and the path ends in the sanitized file name,
// which can still read as an instruction, so all three are untrusted.
type attachmentFileOut struct {
	Path        untrusted  `json:"path" jsonschema:"the absolute path of the new file; untrusted, since it ends in the message's file name"`
	Size        int64      `json:"size" jsonschema:"bytes written"`
	ContentType untrusted  `json:"content_type" jsonschema:"the attachment's MIME type as the message gives it"`
	Filename    *untrusted `json:"filename,omitempty" jsonschema:"the attachment's file name as the message gives it"`
}

func (s *Server) downloadAttachment(ctx context.Context, _ *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, attachmentFileOut, error) {
	root, dir, err := s.outputDir(in.Dir)
	if err != nil {
		return nil, attachmentFileOut{}, s.errorResult(err)
	}
	defer func() { _ = root.Close() }() // a directory handle: a close error loses nothing
	stream, err := s.daemon.Message.DownloadAttachment(ctx, connect.NewRequest(&chainmailv1.DownloadAttachmentRequest{Ref: in.Ref, Part: in.Part}))
	if err != nil {
		return nil, attachmentFileOut{}, s.errorResult(err)
	}
	defer func() { _ = stream.Close() }() // drained or failed by then; Err reports the outcome
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, attachmentFileOut{}, s.errorResult(err)
		}
		return nil, attachmentFileOut{}, s.errorResult(errors.New("the daemon sent no attachment"))
	}
	header := stream.Msg().GetHeader()
	if header == nil {
		return nil, attachmentFileOut{}, s.errorResult(errors.New("the daemon's first message was not the attachment header"))
	}
	path, n, err := saveFile(root, dir, sanitizeName(header.GetFilename().GetUntrusted(), "attachment"), func(w io.Writer) (int64, error) {
		var n int64
		for stream.Receive() {
			k, err := w.Write(stream.Msg().GetData())
			n += int64(k)
			if err != nil {
				return n, err
			}
		}
		return n, stream.Err()
	})
	if err != nil {
		return nil, attachmentFileOut{}, s.errorResult(err)
	}
	out := attachmentFileOut{Path: serverName(path), Size: n, ContentType: serverName(header.GetContentType()), Filename: wrapPtr(header.GetFilename())}
	content, err := untrustedText(fmt.Sprintf("Saved %d bytes. The path, file name and content type came from the message.", n), out)
	if err != nil {
		return nil, attachmentFileOut{}, s.errorResult(err)
	}
	return &mcp.CallToolResult{Content: content}, out, nil
}

func (s *Server) saveRawMessage(ctx context.Context, _ *mcp.CallToolRequest, in saveRawIn) (*mcp.CallToolResult, savedFileOut, error) {
	root, dir, err := s.outputDir(in.Dir)
	if err != nil {
		return nil, savedFileOut{}, s.errorResult(err)
	}
	defer func() { _ = root.Close() }() // a directory handle: a close error loses nothing
	stream, err := s.daemon.Message.GetRaw(ctx, connect.NewRequest(&chainmailv1.GetRawRequest{Ref: in.Ref}))
	if err != nil {
		return nil, savedFileOut{}, s.errorResult(err)
	}
	defer func() { _ = stream.Close() }() // as in downloadAttachment
	path, n, err := saveFile(root, dir, "message.eml", func(w io.Writer) (int64, error) {
		var n int64
		for stream.Receive() {
			k, err := w.Write(stream.Msg().GetData())
			n += int64(k)
			if err != nil {
				return n, err
			}
		}
		return n, stream.Err()
	})
	if err != nil {
		return nil, savedFileOut{}, s.errorResult(err)
	}
	out := savedFileOut{Path: path, Size: n, ContentType: "message/rfc822"}
	return &mcp.CallToolResult{Content: plainText(fmt.Sprintf("Saved %d bytes to %s.", n, path), out)}, out, nil
}
