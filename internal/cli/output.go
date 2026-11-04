package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

var protoJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// writeJSON writes v to w as one line of JSON. HTML characters are left as
// they are, as protojson does, so URLs in hints stay readable.
func writeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode adds the newline
		return fmt.Errorf("encoding output: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

// writeProto writes msg as one line of JSON with snake_case field names.
func writeProto(w io.Writer, msg proto.Message) error {
	raw, err := protoJSON.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	// protojson varies its whitespace on purpose. Compacting makes the output stable.
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return fmt.Errorf("compacting output: %w", err)
	}
	buf.WriteByte('\n')
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// tabwriter buffers rows and reports write errors from Flush, so row writes ignore them.

func accountsTable(w io.Writer, r *chainmailv1.ListAccountsResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "NAME\tPROVIDER\tADDRESS\tAUTH\tSTATE\tDETAIL")
	for _, a := range r.GetAccounts() {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.GetName(), a.GetProvider(), a.GetAddress(), a.GetAuthMethod(), stateLabel(a.GetAuthState()), a.GetAuthDetail())
	}
	return tw.Flush()
}

func checksTable(w io.Writer, r *chainmailv1.TestAccountResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "CHECK\tOK\tREASON\tLATENCY\tERROR")
	for _, row := range []struct {
		name  string
		check *chainmailv1.Check
	}{{"imap", r.GetImap()}, {"smtp", r.GetSmtp()}} {
		_, _ = fmt.Fprintf(tw, "%s\t%t\t%s\t%dms\t%s\n",
			row.name, row.check.GetOk(), row.check.GetReason(), row.check.GetLatencyMs(), row.check.GetError())
	}
	return tw.Flush()
}

func stateLabel(s chainmailv1.AuthState) string {
	switch s {
	case chainmailv1.AuthState_AUTH_STATE_OK:
		return "ok"
	case chainmailv1.AuthState_AUTH_STATE_REQUIRED:
		return "auth required"
	default:
		return "unknown"
	}
}

const messageSeparator = "\n------------------------------------------------------------------------\n\n"

// maxGetRefs is how many refs one get takes, so the omitted-refs note
// suggests commands that work.
const maxGetRefs = 50

func statusTable(w io.Writer, loc *time.Location, r *chainmailv1.GetStatusResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintf(tw, "version\t%s\n", r.GetVersion())
	_, _ = fmt.Fprintf(tw, "started\t%s\n", longDate(r.GetStartedAt(), loc))
	_, _ = fmt.Fprintf(tw, "dev mode\t%t\n", r.GetDevMode())
	_, _ = fmt.Fprintf(tw, "secrets\t%s\n", strings.Join(r.GetSecretNames(), ", "))
	if err := tw.Flush(); err != nil {
		return err
	}
	// The sync and cache tables have their own columns, so each is a table
	// of its own below the daemon's details.
	if len(r.GetSync()) > 0 {
		if err := writeString(w, "\n"); err != nil {
			return err
		}
		if err := syncTable(w, loc, r.GetSync()); err != nil {
			return err
		}
	}
	if len(r.GetCache()) == 0 {
		return nil
	}
	if err := writeString(w, "\n"); err != nil {
		return err
	}
	return cacheAccountsTable(w, r.GetCache())
}

// syncTable prints one line per synced folder, for admin status and watch
// status alike: its mode, state, CONDSTORE, last sync, last sweep and last
// error, with "-" for an empty cell, so a line never ends in padding.
// Folder names come from mail servers, so every value is cleaned for the
// terminal.
func syncTable(w io.Writer, loc *time.Location, folders []*chainmailv1.FolderSyncStatus) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tFOLDER\tMODE\tSTATE\tCONDSTORE\tLAST SYNC\tLAST SWEEP\tERROR")
	for _, f := range folders {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			termText(f.GetAccount()), termText(f.GetFolder()), orDash(termText(f.GetMode())), orDash(termText(f.GetState())),
			f.GetCondstore(), orDash(shortDate(f.GetLastSyncAt(), loc)), orDash(shortDate(f.GetLastSweepAt(), loc)),
			orDash(termText(f.GetLastError())))
	}
	return tw.Flush()
}

// orDash shows an empty cell as "-", so a table never ends a line with padding.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func foldersTable(w io.Writer, r *chainmailv1.ListFoldersResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tFOLDER\tROLE\tMESSAGES\tUNSEEN")
	for _, a := range r.GetAccounts() {
		for _, f := range a.GetFolders() {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", a.GetAccount(), f.GetName(), f.GetRole(), f.GetMessages(), f.GetUnseen())
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return partialLines(w, r.GetPartial())
}

// partialLines reports the accounts and folders a call could not read.
func partialLines(w io.Writer, partial []*chainmailv1.PartialSource) error {
	var b strings.Builder
	for _, p := range partial {
		where := p.GetAccount()
		if p.GetFolder() != "" {
			where += "/" + p.GetFolder()
		}
		fmt.Fprintf(&b, "partial: %s: %s: %s\n", where, p.GetReason(), p.GetMessage())
		writeHint(&b, p.GetHint())
	}
	return writeString(w, b.String())
}

func searchTable(w io.Writer, loc *time.Location, r *chainmailv1.SearchResponse) error {
	snippets := false
	for _, row := range r.GetRows() {
		snippets = snippets || row.GetSnippet() != nil
	}
	tw := newTable(w)
	header := "DATE\tACCOUNT\tFROM\tSUBJECT\tREF"
	if snippets {
		header += "\tSNIPPET"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, row := range r.GetRows() {
		subject := row.GetSubject().GetUntrusted()
		if n := row.GetThreadCount(); n > 1 {
			subject = fmt.Sprintf("%s (%d)", subject, n)
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s", shortDate(row.GetDate(), loc), row.GetAccount(),
			clip(firstSender(row.GetFrom()), 30), clip(subject, 60), row.GetRef())
		if snippets {
			line += "\t" + clip(row.GetSnippet().GetUntrusted(), 60)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := partialLines(w, r.GetPartial()); err != nil {
		return err
	}
	if err := sourceLine(w, r.GetAccountCounts()); err != nil {
		return err
	}
	if next := r.GetNextCursor(); next != "" {
		return writeString(w, "more: --cursor "+next+"\n")
	}
	return nil
}

func countTable(w io.Writer, r *chainmailv1.SearchResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tCOUNT")
	for _, c := range r.GetAccountCounts() {
		_, _ = fmt.Fprintf(tw, "%s\t%d\n", c.GetAccount(), c.GetCount())
	}
	_, _ = fmt.Fprintf(tw, "total\t%d\n", r.GetCount())
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := partialLines(w, r.GetPartial()); err != nil {
		return err
	}
	return sourceLine(w, r.GetAccountCounts())
}

// sourceLine names what answered each account, the cache or its mail
// server, and why a forced cache answer is incomplete. It writes nothing
// when no account says, as a daemon without a cache answers.
func sourceLine(w io.Writer, counts []*chainmailv1.AccountCount) error {
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		if c.GetSource() == "" {
			continue
		}
		part := termText(c.GetAccount()) + " " + termText(c.GetSource())
		if reason := c.GetIncompleteReason(); !c.GetComplete() && reason != "" {
			part += " (incomplete: " + termText(reason) + ")"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil
	}
	return writeString(w, "source: "+strings.Join(parts, ", ")+"\n")
}

// messagesText prints get's items in order. A failed item prints its error
// and hint where its message would be, so the rest of the batch still reads.
func messagesText(w io.Writer, loc *time.Location, items []*chainmailv1.GetItem) error {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString(messageSeparator)
		}
		if e := it.GetError(); e != nil {
			fmt.Fprintf(&b, "error %s: %s: %s\n", it.GetRef(), e.GetReason(), e.GetMessage())
			writeHint(&b, e.GetHint())
			continue
		}
		writeMessageText(&b, loc, it.GetMessage())
	}
	return writeString(w, b.String())
}

// threadText prints a thread oldest first. When the daemon left members
// out, because the thread did not fit its response cap or because a member
// could not be read, a closing note lists the get commands that fetch them,
// with --full when the thread was read with it.
func threadText(w io.Writer, loc *time.Location, r *chainmailv1.GetThreadResponse, full bool) error {
	var b strings.Builder
	for i, m := range r.GetMessages() {
		if i > 0 {
			b.WriteString(messageSeparator)
		}
		writeMessageText(&b, loc, m)
	}
	if omitted := r.GetOmittedRefs(); r.GetTruncated() && len(omitted) > 0 {
		if len(r.GetMessages()) > 0 {
			b.WriteString(messageSeparator)
		}
		noun := "messages"
		if len(omitted) == 1 {
			noun = "message"
		}
		fmt.Fprintf(&b, "[%d %s omitted: too large or unreadable; fetch them with:]\n", len(omitted), noun)
		get := "chainmail get"
		if full {
			get += " --full"
		}
		for batch := range slices.Chunk(omitted, maxGetRefs) {
			fmt.Fprintf(&b, "  %s %s\n", get, strings.Join(batch, " "))
		}
	}
	return writeString(w, b.String())
}

func writeMessageText(b *strings.Builder, loc *time.Location, m *chainmailv1.Message) {
	fmt.Fprintf(b, "From: %s\n", addressList(m.GetFrom()))
	fmt.Fprintf(b, "To: %s\n", addressList(m.GetTo()))
	if len(m.GetCc()) > 0 {
		fmt.Fprintf(b, "Cc: %s\n", addressList(m.GetCc()))
	}
	fmt.Fprintf(b, "Date: %s\n", longDate(m.GetDate(), loc))
	fmt.Fprintf(b, "Subject: %s\n", m.GetSubject().GetUntrusted())
	if len(m.GetLabels()) > 0 {
		fmt.Fprintf(b, "Labels: %s\n", strings.Join(m.GetLabels(), ", "))
	}
	fmt.Fprintf(b, "Ref: %s\n", m.GetRef())
	for _, a := range m.GetAttachments() {
		fmt.Fprintf(b, "Attachment: %s %s (%s, %d bytes)\n", a.GetPart(), a.GetFilename().GetUntrusted(), a.GetContentType(), a.GetSize())
	}
	b.WriteString("\n")
	b.WriteString(m.GetBody().GetUntrusted())
	b.WriteString("\n")
	if m.GetBody().GetQuotedTrimmed() {
		b.WriteString("[quoted text trimmed; --full shows it]\n")
	}
	if m.GetBody().GetTruncated() {
		b.WriteString("[truncated; --full shows the rest]\n")
	}
}

func writeHint(b *strings.Builder, hint string) {
	if hint != "" {
		fmt.Fprintf(b, "  hint: %s\n", hint)
	}
}

func writeString(w io.Writer, s string) error {
	if _, err := io.WriteString(w, s); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func addressList(list []*chainmailv1.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		name, email := a.GetName().GetUntrusted(), a.GetEmail().GetUntrusted()
		if name == "" {
			parts[i] = email
			continue
		}
		parts[i] = name + " <" + email + ">"
	}
	return strings.Join(parts, ", ")
}

func firstSender(list []*chainmailv1.Address) string {
	if len(list) == 0 {
		return ""
	}
	if name := list[0].GetName().GetUntrusted(); name != "" {
		return name
	}
	return list[0].GetEmail().GetUntrusted()
}

// clip shortens s to n characters, marking the cut with "...".
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

func shortDate(ts *timestamppb.Timestamp, loc *time.Location) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().In(loc).Format("2006-01-02 15:04")
}

func longDate(ts *timestamppb.Timestamp, loc *time.Location) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().In(loc).Format("Mon, 02 Jan 2006 15:04 MST")
}

func limitsTable(w io.Writer, loc *time.Location, r *chainmailv1.ProbeLimitsResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tCAP\tPOOL\tPROBED\tERROR")
	for _, l := range r.GetLimits() {
		if e := l.GetError(); e != nil {
			_, _ = fmt.Fprintf(tw, "%s\t\t\t\t%s: %s\n", termText(l.GetAccount()), termText(e.GetReason()), termText(e.GetMessage()))
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t\n", termText(l.GetAccount()), l.GetImapCap(), l.GetPoolSize(), shortDate(l.GetProbedAt(), loc))
	}
	return tw.Flush()
}
