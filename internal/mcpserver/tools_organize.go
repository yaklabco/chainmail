package mcpserver

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

const (
	toolExpunge      = "expunge"
	toolDeleteFolder = "delete_folder"
	// gateTimeout bounds one policy check, so a hung daemon cannot stall Run.
	gateTimeout = 5 * time.Second
)

var (
	organizeIdem = hints{idempotent: true, openWorld: true}
	organizeOnce = hints{openWorld: true}
	gatedHints   = hints{destructive: true, idempotent: true, openWorld: true}
	gatedTools   = []string{toolDeleteFolder, toolExpunge}
)

func (s *Server) registerOrganizeTools() {
	addTool(s, "move", "Move messages", "Move messages to a folder on each message's own account. Use the new_ref of each outcome from now on.", organizeIdem, s.move)
	addTool(s, "copy", "Copy messages", "Copy messages to a folder. new_ref is the copy's ref.", organizeOnce, s.copyMessages)
	addTool(s, "archive", "Archive messages", "Move messages out of the inbox to the account's archive.", organizeIdem, s.archive)
	addTool(s, "trash", "Move messages to Trash", "Move messages to the account's Trash. Nothing is deleted for good.", organizeIdem, s.trash)
	addTool(s, "set_flags", "Set flags", "Add and remove flags: seen, flagged, answered, draft, or keywords.", organizeIdem, s.setFlags)
	addTool(s, "set_labels", "Set Gmail labels", "Add and remove Gmail labels. Other accounts refuse it; use move there.", organizeIdem, s.setLabels)
	addTool(s, "create_folder", "Create a folder", "Create a folder, and its parents when missing. The result is the name the server uses.", organizeIdem, s.createFolder)
	addTool(s, "rename_folder", "Rename a folder", "Rename a folder. The result is the new name the server uses.", organizeOnce, s.renameFolder)
}

type refsIn struct {
	Refs []string `json:"refs" jsonschema:"1 to 500 refs from search, from any accounts"`
}

type moveIn struct {
	Refs   []string `json:"refs" jsonschema:"1 to 500 refs from search, from any accounts"`
	Folder string   `json:"folder" jsonschema:"the destination folder on each message's own account, as list_folders names it"`
}

type flagsIn struct {
	Refs   []string `json:"refs" jsonschema:"1 to 500 refs from search, from any accounts"`
	Add    []string `json:"add,omitempty" jsonschema:"flags to add: seen, flagged, answered, draft, or keywords of A-Z a-z 0-9 $ _ . -"`
	Remove []string `json:"remove,omitempty" jsonschema:"flags to remove, named as in add"`
}

type labelsIn struct {
	Refs   []string `json:"refs" jsonschema:"1 to 500 refs from search, on Google accounts"`
	Add    []string `json:"add,omitempty" jsonschema:"labels to add; a missing label is created; the system labels \\Inbox, \\Important, \\Starred, \\Sent and \\Draft are accepted"`
	Remove []string `json:"remove,omitempty" jsonschema:"labels to remove"`
}

type createFolderIn struct {
	Account string `json:"account" jsonschema:"the account to create it on"`
	Name    string `json:"name" jsonschema:"the full folder name with / between levels; a personal namespace prefix may be left out"`
}

type renameFolderIn struct {
	Account string `json:"account" jsonschema:"the account the folder is on"`
	From    string `json:"from" jsonschema:"the folder's current name, as list_folders names it"`
	To      string `json:"to" jsonschema:"the new name, written as create_folder takes it"`
}

type deleteFolderIn struct {
	Account string `json:"account" jsonschema:"the account the folder is on"`
	Name    string `json:"name" jsonschema:"the folder to delete; INBOX, role folders and folders with subfolders are refused"`
	Force   bool   `json:"force,omitempty" jsonschema:"delete it even when it holds mail, which is deleted with it (on Google the messages stay in All Mail)"`
}

type folderNameOut struct {
	Account string `json:"account"`
	Name    string `json:"name" jsonschema:"the folder's name on the server, which later calls take"`
}

type deleteFolderOut struct {
	Account string `json:"account"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

type outcomeOut struct {
	Ref    string     `json:"ref"`
	NewRef string     `json:"new_ref,omitempty" jsonschema:"the message's ref afterwards, or for copy the copy's ref; use it from now on"`
	Error  *errorBody `json:"error,omitempty" jsonschema:"set when this ref failed; the others still ran"`
}

type outcomesOut struct {
	Outcomes  []outcomeOut `json:"outcomes" jsonschema:"one per ref, in the order given"`
	Succeeded int          `json:"succeeded"`
	Failed    int          `json:"failed"`
}

// outcomes summarizes a batch. A batch where every ref failed is an error
// result with the first failure's reason.
func (s *Server) outcomes(verb string, outs []*chainmailv1.RefOutcome) (*mcp.CallToolResult, outcomesOut, error) {
	out := outcomesOut{Outcomes: make([]outcomeOut, 0, len(outs))}
	var first *errorBody
	for _, o := range outs {
		e := itemError(o.GetError())
		out.Outcomes = append(out.Outcomes, outcomeOut{Ref: o.GetRef(), NewRef: o.GetNewRef(), Error: e})
		switch {
		case e == nil:
			out.Succeeded++
		case first == nil:
			first = e
			out.Failed++
		default:
			out.Failed++
		}
	}
	if out.Succeeded == 0 && first != nil {
		body := *first
		body.Message = fmt.Sprintf("all %d refs failed; the first: %s", out.Failed, first.Message)
		return nil, outcomesOut{}, &toolError{body: body}
	}
	return &mcp.CallToolResult{Content: plainText(fmt.Sprintf("%s %d, failed %d", verb, out.Succeeded, out.Failed), out)}, out, nil
}

func imapFlags(fs []string) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = imapFlag(f)
	}
	return out
}

func (s *Server) move(ctx context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.Move(ctx, connect.NewRequest(&chainmailv1.MoveRequest{Refs: in.Refs, Folder: in.Folder}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("moved", resp.Msg.GetOutcomes())
}

func (s *Server) copyMessages(ctx context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.Copy(ctx, connect.NewRequest(&chainmailv1.CopyRequest{Refs: in.Refs, Folder: in.Folder}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("copied", resp.Msg.GetOutcomes())
}

func (s *Server) archive(ctx context.Context, _ *mcp.CallToolRequest, in refsIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.Archive(ctx, connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: in.Refs}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("archived", resp.Msg.GetOutcomes())
}

func (s *Server) trash(ctx context.Context, _ *mcp.CallToolRequest, in refsIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.Trash(ctx, connect.NewRequest(&chainmailv1.TrashRequest{Refs: in.Refs}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("trashed", resp.Msg.GetOutcomes())
}

func (s *Server) expunge(ctx context.Context, _ *mcp.CallToolRequest, in refsIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.Expunge(ctx, connect.NewRequest(&chainmailv1.ExpungeRequest{Refs: in.Refs}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("expunged", resp.Msg.GetOutcomes())
}

func (s *Server) setFlags(ctx context.Context, _ *mcp.CallToolRequest, in flagsIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.SetFlags(ctx, connect.NewRequest(&chainmailv1.SetFlagsRequest{Refs: in.Refs, Add: imapFlags(in.Add), Remove: imapFlags(in.Remove)}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("updated", resp.Msg.GetOutcomes())
}

func (s *Server) setLabels(ctx context.Context, _ *mcp.CallToolRequest, in labelsIn) (*mcp.CallToolResult, outcomesOut, error) {
	resp, err := s.daemon.Organize.SetLabels(ctx, connect.NewRequest(&chainmailv1.SetLabelsRequest{Refs: in.Refs, Add: in.Add, Remove: in.Remove}))
	if err != nil {
		return nil, outcomesOut{}, s.errorResult(err)
	}
	return s.outcomes("labelled", resp.Msg.GetOutcomes())
}

func (s *Server) createFolder(ctx context.Context, _ *mcp.CallToolRequest, in createFolderIn) (*mcp.CallToolResult, folderNameOut, error) {
	resp, err := s.daemon.Organize.CreateFolder(ctx, connect.NewRequest(&chainmailv1.CreateFolderRequest{Account: in.Account, Name: in.Name}))
	if err != nil {
		return nil, folderNameOut{}, s.errorResult(err)
	}
	out := folderNameOut{Account: in.Account, Name: resp.Msg.GetName()}
	return &mcp.CallToolResult{Content: plainText("Created "+out.Name+".", out)}, out, nil
}

func (s *Server) renameFolder(ctx context.Context, _ *mcp.CallToolRequest, in renameFolderIn) (*mcp.CallToolResult, folderNameOut, error) {
	resp, err := s.daemon.Organize.RenameFolder(ctx, connect.NewRequest(&chainmailv1.RenameFolderRequest{Account: in.Account, From: in.From, To: in.To}))
	if err != nil {
		return nil, folderNameOut{}, s.errorResult(err)
	}
	out := folderNameOut{Account: in.Account, Name: resp.Msg.GetName()}
	return &mcp.CallToolResult{Content: plainText("Renamed to "+out.Name+".", out)}, out, nil
}

func (s *Server) deleteFolder(ctx context.Context, _ *mcp.CallToolRequest, in deleteFolderIn) (*mcp.CallToolResult, deleteFolderOut, error) {
	if _, err := s.daemon.Organize.DeleteFolder(ctx, connect.NewRequest(&chainmailv1.DeleteFolderRequest{Account: in.Account, Name: in.Name, Force: in.Force})); err != nil {
		return nil, deleteFolderOut{}, s.errorResult(err)
	}
	out := deleteFolderOut{Account: in.Account, Name: in.Name, Deleted: true}
	return &mcp.CallToolResult{Content: plainText("Deleted "+in.Name+".", out)}, out, nil
}

func (s *Server) addGated(name string) {
	switch name {
	case toolExpunge:
		addTool(s, toolExpunge, "Expunge messages",
			"Delete messages for good. Listed only while policy allows expunge on some account; the daemon still checks each message's account and refuses with POLICY_DENIED.",
			gatedHints, s.expunge)
	case toolDeleteFolder:
		addTool(s, toolDeleteFolder, "Delete a folder",
			"Delete a folder. Listed only while policy allows folder deletion on some account; the daemon still checks the account and refuses with POLICY_DENIED.",
			gatedHints, s.deleteFolder)
	}
}

// refreshGate lists expunge and delete_folder when policy allows them on at
// least one account, and removes them otherwise or when the daemon cannot
// be asked. It always asks the daemon, waiting for any refresh in flight
// first. Run calls it before serving.
func (s *Server) refreshGate(ctx context.Context) {
	s.gate.refresh <- struct{}{}
	defer func() { <-s.gate.refresh }()
	s.checkGate(ctx)
}

// refreshStaleGate runs a refresh before a tools/list once the last check is
// older than gateInterval. Lists that find the check stale together share
// one daemon call: each waits for the refresh in flight and then looks
// again, so only the first asks. A list whose ctx ends while it waits skips
// the refresh and is served from the current state.
func (s *Server) refreshStaleGate(ctx context.Context) {
	if !s.gateStale() {
		return
	}
	select {
	case s.gate.refresh <- struct{}{}:
	default:
		if s.gate.contended != nil {
			s.gate.contended()
		}
		select {
		case s.gate.refresh <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	defer func() { <-s.gate.refresh }()
	// Stale is false now when a refresh finished while this one waited.
	if ctx.Err() == nil && s.gateStale() {
		s.checkGate(ctx)
	}
}

// checkGate asks the daemon which gated tools policy allows and applies the
// answer. The caller holds s.gate.refresh, so answers apply in the order
// they were asked for. The daemon call runs without s.gate.mu, and the tool
// changes and the check time are applied together under it. AddTool and
// RemoveTools only update the SDK registry here: the SDK sends
// notifications/tools/list_changed later, from a timer, without its lock or
// s.gate.mu held.
func (s *Server) checkGate(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, gateTimeout)
	defer cancel()
	want := map[string]bool{}
	resp, err := s.daemon.Account.ListAccounts(ctx, connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		// Only the kind: the text of a transport or detail-less error can
		// carry a backend's diagnostics or a secret (see errorKind).
		s.opts.Logger.Warn("checking policy for the gated tools", "kind", errorKind(err))
	} else {
		for _, a := range resp.Msg.GetAccounts() {
			want[toolExpunge] = want[toolExpunge] || a.GetCapabilities().GetAllowExpunge()
			want[toolDeleteFolder] = want[toolDeleteFolder] || a.GetCapabilities().GetAllowFolderDelete()
		}
	}
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	s.gate.checked = s.now()
	for _, name := range gatedTools {
		switch {
		case want[name] && !s.gate.listed[name]:
			s.addGated(name)
		case !want[name] && s.gate.listed[name]:
			s.mcp.RemoveTools(name)
		}
		s.gate.listed[name] = want[name]
	}
}
