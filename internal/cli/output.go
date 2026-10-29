package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

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
