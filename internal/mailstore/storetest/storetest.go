// Package storetest is the conformance suite every mailstore.Store runs:
// the in-memory fake, imapstore against Dovecot and imapstore against
// fakegmail. Each case drives the Store through its own methods and checks
// what the Store itself reports afterwards, so the fake and the IMAP store
// cannot drift apart. The write cases are the rows of spec 7.1 for the
// harness's mode, standard IMAP or Google, and the read cases are SP2's:
// ListFolders, Search, FetchSummaries, FetchMessages, StreamPart and Thread.
//
// A standard harness's account has the folders INBOX, Sent, Drafts, Archive,
// Trash and Junk with those roles, "/" as its hierarchy delimiter, and no
// archive_folder setting unless a case's Config sets one. A Google
// harness's account has INBOX and [Gmail]/All Mail, Sent Mail, Trash and
// Spam.
//
// Rows of 7.1 that turn on what the server advertises or answers cannot be
// set up through every harness, since the fake has no capabilities: a
// server without MOVE or UIDPLUS, a transfer acknowledged without COPYUID,
// an expunge that fails after a confirmed copy, a refused Gmail label
// addition, and Gmail system folders that are hidden, localised or under
// [Google Mail]. imapstore's scripted-server tests cover those.
package storetest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Gmail's own folders, as a Google harness names them.
const (
	GmailAllMail = "[Gmail]/All Mail"
	GmailSent    = "[Gmail]/Sent Mail"
	GmailTrash   = "[Gmail]/Trash"
	GmailSpam    = "[Gmail]/Spam"
)

// Attachment is an attachment of a seeded message.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// AttachmentPart is the IMAP section number of a seeded message's i-th
// attachment, counting from 0: the text is part 1 of a multipart/mixed
// message and the attachments follow it.
func AttachmentPart(i int) string { return strconv.Itoa(i + 2) }

// Message is a message to seed. Raw gives it as MIME, for harnesses that
// APPEND it; the fake takes the fields directly.
type Message struct {
	// Folder is where the message goes. On Google accounts it is All Mail,
	// the default when empty, Trash or Spam, and Labels put it in label
	// folders.
	Folder string
	// Labels are Gmail labels, such as `\Inbox` or Work. A user label the
	// account lacks is created. Standard harnesses ignore them.
	Labels []string
	Flags  []string
	// Date is both the Date header and the internal date.
	Date time.Time
	// From is an address.
	From string
	// Cc and Bcc are addresses; empty means none.
	Cc        string
	Bcc       string
	Subject   string
	MessageID string // without angle brackets
	InReplyTo string // without angle brackets
	// References holds Message-IDs without angle brackets, oldest first.
	References  []string
	Body        string
	Attachments []Attachment
}

// Raw returns m as a MIME message: text/plain, or multipart/mixed with the
// text first and each attachment base64-encoded on one line, so the size a
// server reports for an attachment part is the length of its base64 text.
func (m Message) Raw() []byte {
	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", m.From)
	header("To", "me@example.test")
	if m.Cc != "" {
		header("Cc", m.Cc)
	}
	if m.Bcc != "" {
		header("Bcc", m.Bcc)
	}
	header("Subject", m.Subject)
	header("Date", m.Date.Format(time.RFC1123Z))
	if m.MessageID != "" {
		header("Message-ID", "<"+m.MessageID+">")
	}
	if m.InReplyTo != "" {
		header("In-Reply-To", "<"+m.InReplyTo+">")
	}
	if len(m.References) > 0 {
		header("References", "<"+strings.Join(m.References, "> <")+">")
	}
	header("MIME-Version", "1.0")
	if len(m.Attachments) == 0 {
		header("Content-Type", "text/plain; charset=utf-8")
		b.WriteString("\r\n" + m.Body + "\r\n")
		return b.Bytes()
	}
	const boundary = "conformance-boundary"
	header("Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	b.WriteString("\r\n--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + m.Body + "\r\n")
	for _, a := range m.Attachments {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; name=%q\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			boundary, a.ContentType, a.Filename, a.Filename, base64.StdEncoding.EncodeToString(a.Data))
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

// Seeder adds messages to the account through the backend itself, not
// through the Store under test.
type Seeder func(t *testing.T, msgs ...Message)

// Config is the account configuration a case asks for. The zero Config is
// the account the package comment describes.
type Config struct {
	// ArchiveFolder is the account's archive_folder setting: where Archive
	// moves mail on standard accounts, in place of the folder with the
	// \Archive role. Google accounts archive by label and ignore it. The
	// case creates the folder before it archives.
	ArchiveFolder string
	// Namespace, when its Prefix is set, is the personal namespace the
	// account keeps every folder under, as a Dovecot server such as
	// Hostinger's keeps them under "INBOX." with "." between levels. Only a
	// harness with Namespaces set is asked for one.
	Namespace Namespace
}

// Namespace is an account's personal namespace: the prefix its folders sit
// under and the delimiter between levels. The zero Namespace is the one the
// package comment describes, no prefix and "/".
type Namespace struct {
	Prefix string
	Delim  string
}

// serverName is the name CreateFolder gives a folder asked for as name in
// the namespace: "/" written as its delimiter, and under its prefix unless
// name is INBOX or starts with the prefix already.
func (n Namespace) serverName(name string) string {
	if n.Delim != "" {
		name = strings.ReplaceAll(name, "/", n.Delim)
	}
	if n.Prefix == "" || strings.EqualFold(name, "INBOX") || strings.HasPrefix(name, n.Prefix) {
		return name
	}
	return n.Prefix + name
}

// Harness makes a fresh account for each case.
type Harness struct {
	// New returns a Store on an account of the case's own, configured as
	// cfg says, and a Seeder for it. The harness closes the Store when t
	// ends.
	New func(t *testing.T, cfg Config) (mailstore.Store, Seeder)
	// Gmail says the account is a Google account.
	Gmail bool
	// Namespaces says New makes an account under Config.Namespace when it
	// is set. Other harnesses run the namespace case on their own account
	// only.
	Namespaces bool
}

// Run runs every case for the harness's mode, each on a fresh account.
func Run(t *testing.T, h Harness) {
	t.Helper()
	for _, c := range cases {
		if (c.mode == standardOnly && h.Gmail) || (c.mode == gmailOnly && !h.Gmail) {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			e := &env{t: t, ctx: t.Context(), gmail: h.Gmail, namespaces: h.Namespaces}
			e.open = func(cfg Config) { e.s, e.seed = h.New(t, cfg) }
			e.open(Config{})
			c.run(e)
		})
	}
}

type mode int

const (
	both mode = iota
	standardOnly
	gmailOnly
)

type testCase struct {
	name string
	mode mode
	run  func(e *env)
}

// env is one case's account.
type env struct {
	t     *testing.T
	ctx   context.Context
	s     mailstore.Store
	seed  Seeder
	gmail bool
	// namespaces says open can make an account under a namespace prefix.
	namespaces bool
	// open replaces s and seed with a fresh account configured as cfg says.
	open func(cfg Config)
}

// day is a fixed date in October 2026.
func day(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }

// msg is a message whose subject is token, which no other message's
// subject contains, in the account's main mailbox: INBOX, or on Google All
// Mail with the \Inbox label.
func (e *env) msg(token string) Message {
	m := Message{
		Folder: "INBOX", Date: day(20), From: "alice@example.com", Subject: token,
		MessageID: strings.ToLower(token) + "@conformance.test", Body: "Body of " + token + ".",
	}
	if e.gmail {
		m.Folder, m.Labels = GmailAllMail, []string{`\Inbox`}
	}
	return m
}

// in returns m in folder: on Google accounts a label, or All Mail, Trash or Spam.
func (e *env) in(m Message, folder string) Message {
	if !e.gmail {
		m.Folder = folder
		return m
	}
	switch folder {
	case GmailAllMail, GmailTrash, GmailSpam:
		m.Folder, m.Labels = folder, nil
	case "INBOX":
		m.Folder, m.Labels = GmailAllMail, []string{`\Inbox`}
	default:
		m.Folder, m.Labels = GmailAllMail, []string{folder}
	}
	return m
}

// Folder names for the account's mode.
func (e *env) trash() string   { return pick(e.gmail, GmailTrash, "Trash") }
func (e *env) spam() string    { return pick(e.gmail, GmailSpam, "Junk") }
func (e *env) home() string    { return pick(e.gmail, GmailAllMail, "INBOX") }
func (e *env) archive() string { return pick(e.gmail, GmailAllMail, "Archive") }

func pick(gmail bool, g, s string) string {
	if gmail {
		return g
	}
	return s
}

// search runs a search for subject:token with in.
func (e *env) search(in, token string, countOnly bool) []mailstore.SourceResult {
	e.t.Helper()
	n, err := query.Parse("subject:" + token)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.s.Search(e.ctx, mailstore.SearchRequest{Query: n, In: in, CountOnly: countOnly})
	if err != nil {
		e.t.Fatalf("Search(subject:%s in:%q): %v", token, in, err)
	}
	for _, r := range res {
		if r.Err != nil {
			e.t.Fatalf("Search(subject:%s in:%q) in %s: %v", token, in, r.Source.Folder, r.Err)
		}
	}
	return res
}

// matches returns the summaries of the messages whose subject holds token,
// searched with in.
func (e *env) matches(in, token string) []mailstore.Summary {
	e.t.Helper()
	var out []mailstore.Summary
	for _, r := range e.search(in, token, false) {
		if len(r.UIDs) == 0 {
			continue
		}
		sums, err := e.s.FetchSummaries(e.ctx, r.Source, r.UIDs, mailstore.SummaryOptions{})
		if err != nil {
			e.t.Fatalf("FetchSummaries(%s): %v", r.Source.Folder, err)
		}
		out = append(out, sums...)
	}
	return out
}

// only returns the summary of the one message whose subject is token,
// searched with in.
func (e *env) only(in, token string) mailstore.Summary {
	e.t.Helper()
	got := named(e.matches(in, token), token)
	if len(got) != 1 {
		e.t.Fatalf("subject:%s in:%q found %d messages, want 1: %v", token, in, len(got), refsOf(got))
	}
	return got[0]
}

// named keeps the summaries whose subject is token: a search for
// subject:token also finds longer subjects that hold it.
func named(sums []mailstore.Summary, token string) []mailstore.Summary {
	return slices.DeleteFunc(sums, func(s mailstore.Summary) bool { return s.Subject != token })
}

// ref returns the ref of the one message whose subject is token, in the
// account's main mailbox, or with in when given.
func (e *env) ref(token string, in ...string) ref.Ref {
	e.t.Helper()
	return e.only(strings.Join(in, ""), token).Ref
}

// where returns the folders holding a message whose subject is token,
// once per message, sorted. On Google accounts that is All Mail, Trash or
// Spam.
func (e *env) where(token string) []string {
	e.t.Helper()
	var out []string
	for _, s := range named(e.matches("anywhere", token), token) {
		out = append(out, s.Ref.Folder)
	}
	slices.Sort(out)
	return out
}

// labels returns the Gmail labels of the one message whose subject is token,
// found with in.
func (e *env) labels(token string, in ...string) []string {
	e.t.Helper()
	return e.only(strings.Join(in, ""), token).Labels
}

// fetch reads one message.
func (e *env) fetch(r ref.Ref) (*mailstore.Message, error) {
	e.t.Helper()
	res := e.s.FetchMessages(e.ctx, []ref.Ref{r})
	if len(res) != 1 {
		e.t.Fatalf("FetchMessages gave %d results for 1 ref", len(res))
	}
	return res[0].Message, res[0].Err
}

// readable checks that r reads as the message whose subject is token.
func (e *env) readable(r ref.Ref, token string) {
	e.t.Helper()
	m, err := e.fetch(r)
	if err != nil {
		e.t.Errorf("FetchMessages(%v): %v, want message %s", r, err, token)
		return
	}
	if m.Summary.Subject != token {
		e.t.Errorf("FetchMessages(%v) subject = %q, want %q", r, m.Summary.Subject, token)
	}
}

// stale checks that r no longer reads.
func (e *env) stale(r ref.Ref) {
	e.t.Helper()
	if _, err := e.fetch(r); !errors.Is(err, mailstore.ErrStaleRef) {
		e.t.Errorf("FetchMessages(%v) = %v, want ErrStaleRef", r, err)
	}
}

// folders returns the account's folder names.
func (e *env) folders() []string {
	e.t.Helper()
	list, err := e.s.ListFolders(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(list))
	for i, f := range list {
		out[i] = f.Name
	}
	return out
}

// mustCreate creates name, which the account keeps as it is.
func (e *env) mustCreate(name string) {
	e.t.Helper()
	got, err := e.s.CreateFolder(e.ctx, name)
	if err != nil {
		e.t.Fatalf("CreateFolder(%q): %v", name, err)
	}
	if got != name {
		e.t.Errorf("CreateFolder(%q) = %q, want the name as given", name, got)
	}
}

// errOf is the error of a call that also returns a name.
func errOf(_ string, err error) error { return err }

// foreign is r as a ref of another account.
func foreign(r ref.Ref) ref.Ref {
	r.Account = "conformance-other-account"
	return r
}

func one(t *testing.T, res []mailstore.RefResult) mailstore.RefResult {
	t.Helper()
	if len(res) != 1 {
		t.Fatalf("got %d results for 1 ref", len(res))
	}
	return res[0]
}

func refsOf(sums []mailstore.Summary) []ref.Ref {
	out := make([]ref.Ref, len(sums))
	for i, s := range sums {
		out[i] = s.Ref
	}
	return out
}

func hasFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func bare(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
}

// checkPlaced checks a write's result for a message now in folder under a
// ref other than old, readable as token.
func (e *env) checkPlaced(got mailstore.RefResult, old ref.Ref, folder, token string) {
	e.t.Helper()
	if got.Err != nil {
		e.t.Errorf("result = %v, want a ref in %s", got.Err, folder)
		return
	}
	if got.Ref.Folder != folder || got.Ref.UID == 0 || got.Ref.UIDValidity == 0 || got.Ref == old {
		e.t.Errorf("result ref = %v, want a new ref in %s (was %v)", got.Ref, folder, old)
		return
	}
	if e.gmail && got.Ref.GmailMsgID != old.GmailMsgID {
		e.t.Errorf("result ref = %v, want it to keep X-GM-MSGID %d", got.Ref, old.GmailMsgID)
	}
	e.readable(got.Ref, token)
}

// checkKept checks a write's result for a message whose ref stays r.
func (e *env) checkKept(got mailstore.RefResult, r ref.Ref) {
	e.t.Helper()
	if got != (mailstore.RefResult{Ref: r}) {
		e.t.Errorf("result = %+v, want the ref unchanged: %v", got, r)
	}
}

func (e *env) wantWhere(token string, want ...string) {
	e.t.Helper()
	slices.Sort(want)
	if got := e.where(token); !slices.Equal(got, want) {
		e.t.Errorf("%s is in %q, want %q", token, got, want)
	}
}

// unread checks that the message whose subject is token, found with in, is
// not \Seen: reads never set it.
func (e *env) unread(token string, in ...string) {
	e.t.Helper()
	if got := e.only(strings.Join(in, ""), token).Flags; hasFold(got, `\Seen`) {
		e.t.Errorf("%s flags = %q after reading it, want no \\Seen: reads never set it", token, got)
	}
}

// bumped is r with another UIDVALIDITY, as a ref reads once its folder's
// UIDVALIDITY has changed.
func bumped(r ref.Ref) ref.Ref {
	r.UIDValidity++
	return r
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s = %v, want %v", what, err, want)
	}
}

var cases = []testCase{
	{"Append returns a readable ref", both, appendReadable},
	{"Append to a missing folder is not found", both, appendMissingFolder},
	{"Append refuses a flag IMAP cannot store", both, appendBadFlag},
	{"Append on Gmail gives a ref an edit can follow", gmailOnly, appendThenEditGmail},
	{"Append and FindByMessageID refs carry X-GM-MSGID", gmailOnly, appendIdentityGmail},
	{"Move gives new refs and the originals go", standardOnly, moveStandard},
	{"Move into the folder a ref names keeps it", both, moveIntoOwnFolder},
	{"Move to a missing folder changes nothing", both, moveMissingFolder},
	{"Move fails per ref", both, moveFailsPerRef},
	{"Move to a label edits labels and keeps the ref", gmailOnly, moveToLabelGmail},
	{"Move to INBOX adds Inbox and keeps the ref", gmailOnly, moveToInboxGmail},
	{"Move to Trash or Spam gives a ref there", gmailOnly, moveToBinGmail},
	{"Move from Trash goes back to All Mail with the label", gmailOnly, moveFromTrashGmail},
	{"Move from Spam to INBOX and from Trash to All Mail", gmailOnly, moveBackGmail},
	{"Copy keeps the original", standardOnly, copyStandard},
	{"Copy into its own folder duplicates", standardOnly, copyIntoOwnFolder},
	{"Copy to a label adds it and keeps the ref", gmailOnly, copyToLabelGmail},
	{"Copy to Trash or Spam moves there, as Gmail does", gmailOnly, copyToBinGmail},
	{"Copy from Trash or Spam to All Mail is unsupported", gmailOnly, copyFromBinToAllMailGmail},
	{"Archive", both, archive},
	{"Archive goes to archive_folder when it is set", both, archiveConfigured},
	{"Trash", both, trash},
	{"Expunge deletes only the given messages", both, expunge},
	{"StoreFlags adds and removes", both, storeFlags},
	{"StoreFlags refuses a flag IMAP cannot store", both, storeFlagsBad},
	{"StoreLabels is unsupported off Gmail", standardOnly, storeLabelsStandard},
	{"StoreLabels edits labels and creates new ones", gmailOnly, storeLabelsGmail},
	{"StoreLabels refuses what is not a label", gmailOnly, storeLabelsRefused},
	{"CreateFolder creates parents and refuses bad names", both, createFolder},
	{"RenameFolder moves children and mail", both, renameFolder},
	{"RenameFolder refuses system folders and bad targets", both, renameRefused},
	{"DeleteFolder follows the folder rules", both, deleteFolder},
	{"CreateFolder and RenameFolder name folders as the server does", both, folderNamespace},
	{"FindByMessageID", both, findByMessageID},
	{"ListFolders order, roles and counts", both, listFolders},
	{"Search sources and counts", both, searchBasics},
	{"FetchSummaries keeps order and drops gone UIDs", both, fetchSummaries},
	{"FetchMessages reads headers and attachments", both, fetchMessages},
	{"StreamPart streams the message and its parts", both, streamPart},
	{"Thread", both, thread},
	{"FetchHeaders keeps order, drops gone UIDs and never sets \\Seen", both, fetchHeaders},
	{"SearchFolders names the folders Search uses", both, searchFolders},
}

func appendReadable(e *env) {
	m := e.msg("CfAppend")
	r, err := e.s.Append(e.ctx, "INBOX", []string{`\Seen`}, m.Date, m.Raw())
	if err != nil {
		e.t.Fatalf("Append: %v", err)
	}
	if r.Folder != "INBOX" || r.UIDValidity == 0 || r.UID == 0 {
		e.t.Errorf("Append ref = %v, want one in INBOX", r)
	}
	got, err := e.fetch(r)
	if err != nil {
		e.t.Fatalf("FetchMessages(append ref): %v", err)
	}
	if got.Summary.Subject != "CfAppend" || bare(got.MessageID) != m.MessageID {
		e.t.Errorf("appended message reads as %q %q", got.Summary.Subject, got.MessageID)
	}
	if !slices.Contains(got.Summary.Flags, `\Seen`) {
		e.t.Errorf("flags = %q, want \\Seen", got.Summary.Flags)
	}
	if !got.Summary.InternalDate.Equal(m.Date) {
		e.t.Errorf("internal date = %v, want %v", got.Summary.InternalDate, m.Date)
	}
	found := e.only("", "CfAppend")
	if e.gmail {
		if found.Ref.Folder != GmailAllMail || !hasFold(found.Labels, `\Inbox`) {
			e.t.Errorf("search found %v with labels %q, want it in All Mail labelled \\Inbox", found.Ref, found.Labels)
		}
		return
	}
	if found.Ref != r {
		e.t.Errorf("search found %v, want the append ref %v", found.Ref, r)
	}
}

func appendMissingFolder(e *env) {
	m := e.msg("CfAppendMissing")
	_, err := e.s.Append(e.ctx, "Conformance Missing", nil, m.Date, m.Raw())
	wantErr(e.t, "Append to a missing folder", err, mailstore.ErrNotFound)
}

func appendBadFlag(e *env) {
	m := e.msg("CfAppendFlag")
	_, err := e.s.Append(e.ctx, "INBOX", []string{`\Recent`}, m.Date, m.Raw())
	wantErr(e.t, `Append with \Recent`, err, mailstore.ErrUnsupported)
	if got := e.matches("anywhere", "CfAppendFlag"); len(got) != 0 {
		e.t.Errorf("a refused Append stored %v", refsOf(got))
	}
}

// appendThenEditGmail: Append to INBOX on Gmail gives a ref in INBOX. An
// edit that keeps the message there keeps the ref, and one that takes it
// out gives its All Mail ref.
func appendThenEditGmail(e *env) {
	m := e.msg("CfAppendEdit")
	r, err := e.s.Append(e.ctx, "INBOX", nil, m.Date, m.Raw())
	if err != nil {
		e.t.Fatal(err)
	}
	e.checkKept(one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{r}, []string{`\Seen`}, nil)), r)
	found, err := e.s.FindByMessageID(e.ctx, "INBOX", m.MessageID)
	if err != nil || len(found) != 1 || found[0] != r {
		e.t.Errorf("FindByMessageID(INBOX) = %v, %v; want the append ref %v", found, err, r)
	}
	got := one(e.t, e.s.Archive(e.ctx, []ref.Ref{r}))
	all := e.only("", "CfAppendEdit")
	if got.Err != nil || got.Ref.Folder != GmailAllMail || got.Ref.UID != all.Ref.UID || got.Ref.UIDValidity != all.Ref.UIDValidity {
		e.t.Errorf("Archive of the INBOX ref = %+v, want its All Mail ref %v", got, all.Ref)
	}
	if hasFold(all.Labels, `\Inbox`) {
		e.t.Errorf("labels = %q after Archive", all.Labels)
	}
	e.readable(got.Ref, "CfAppendEdit")
}

// appendIdentityGmail: on Google every ref carries the message's
// X-GM-MSGID, the refs Append and FindByMessageID return too, though
// APPENDUID and SEARCH do not give it. So once the UIDVALIDITY they name
// changes they are found again, as a ref from a search is, and writes keep
// it on the new refs they give.
func appendIdentityGmail(e *env) {
	m := e.in(e.msg("CfAppendID"), GmailAllMail)
	r, err := e.s.Append(e.ctx, GmailAllMail, nil, m.Date, m.Raw())
	if err != nil {
		e.t.Fatalf("Append: %v", err)
	}
	searched := e.ref("CfAppendID")
	if searched.GmailMsgID == 0 {
		e.t.Errorf("search ref %v, want it to carry X-GM-MSGID", searched)
	}
	if r != searched {
		e.t.Errorf("Append ref = %v, want %v, the ref a search gives, X-GM-MSGID included", r, searched)
	}
	found, err := e.s.FindByMessageID(e.ctx, GmailAllMail, m.MessageID)
	if err != nil || len(found) != 1 || found[0] != searched {
		e.t.Errorf("FindByMessageID = %v, %v; want [%v], X-GM-MSGID included", found, err, searched)
	}
	e.readable(bumped(r), "CfAppendID")
	if got := one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{bumped(r)}, []string{`\Flagged`}, nil)); got.Err != nil {
		e.t.Errorf("StoreFlags of the Append ref under another UIDVALIDITY = %v, want it found by X-GM-MSGID", got.Err)
	}
	if got := e.only("", "CfAppendID").Flags; !hasFold(got, `\Flagged`) {
		e.t.Errorf(`flags = %q, want \Flagged from the StoreFlags of the bumped ref`, got)
	}
	trashed := one(e.t, e.s.Trash(e.ctx, []ref.Ref{r}))
	e.checkPlaced(trashed, r, GmailTrash, "CfAppendID")
	back := one(e.t, e.s.Move(e.ctx, []ref.Ref{trashed.Ref}, "INBOX"))
	e.checkPlaced(back, trashed.Ref, GmailAllMail, "CfAppendID")
}

func moveStandard(e *env) {
	e.seed(e.t, e.msg("CfMoveA"), e.msg("CfMoveB"))
	a, b := e.ref("CfMoveA"), e.ref("CfMoveB")
	res := e.s.Move(e.ctx, []ref.Ref{a, b}, "Archive")
	if len(res) != 2 {
		e.t.Fatalf("Move gave %d results", len(res))
	}
	e.checkPlaced(res[0], a, "Archive", "CfMoveA")
	e.checkPlaced(res[1], b, "Archive", "CfMoveB")
	e.stale(a)
	e.wantWhere("CfMoveA", "Archive")
	e.wantWhere("CfMoveB", "Archive")
}

func moveIntoOwnFolder(e *env) {
	e.seed(e.t, e.msg("CfOwn"), e.in(e.msg("CfOwnTrash"), e.trash()))
	r := e.ref("CfOwn")
	e.checkKept(one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, e.home())), r)
	e.wantWhere("CfOwn", e.home())
	t := e.ref("CfOwnTrash", "trash")
	e.checkKept(one(e.t, e.s.Move(e.ctx, []ref.Ref{t}, e.trash())), t)
	e.wantWhere("CfOwnTrash", e.trash())
}

func moveMissingFolder(e *env) {
	e.seed(e.t, e.msg("CfMoveMissing"))
	r := e.ref("CfMoveMissing")
	wantErr(e.t, "Move to a missing folder", one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, "Conformance Missing")).Err, mailstore.ErrNotFound)
	e.wantWhere("CfMoveMissing", e.home())
	e.readable(r, "CfMoveMissing")
}

func moveFailsPerRef(e *env) {
	e.seed(e.t, e.msg("CfPerRefGone"), e.msg("CfPerRefOK"))
	gone, ok := e.ref("CfPerRefGone"), e.ref("CfPerRefOK")
	if got := one(e.t, e.s.Expunge(e.ctx, []ref.Ref{gone})); got.Err != nil {
		e.t.Fatal(got.Err)
	}
	dest := pick(e.gmail, e.trash(), "Archive")
	res := e.s.Move(e.ctx, []ref.Ref{gone, ok, foreign(ok)}, dest)
	if len(res) != 3 {
		e.t.Fatalf("Move gave %d results", len(res))
	}
	wantErr(e.t, "Move of an expunged ref", res[0].Err, mailstore.ErrStaleRef)
	e.checkPlaced(res[1], ok, dest, "CfPerRefOK")
	wantErr(e.t, "Move of another account's ref", res[2].Err, mailstore.ErrNotFound)
}

// gmailWithWork seeds a message labelled Work, so the Work label exists,
// and returns the token of a message in the inbox.
func (e *env) gmailWithWork(token string) {
	e.t.Helper()
	e.seed(e.t, e.msg(token), e.in(e.msg(token+"Work"), "Work"))
}

func moveToLabelGmail(e *env) {
	e.gmailWithWork("CfToLabel")
	r := e.ref("CfToLabel")
	e.checkKept(one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, "Work")), r)
	if got := e.labels("CfToLabel"); !hasFold(got, "Work") || hasFold(got, `\Inbox`) {
		e.t.Errorf("labels = %q, want Work and not \\Inbox", got)
	}
	e.wantWhere("CfToLabel", GmailAllMail)
}

func moveToInboxGmail(e *env) {
	e.seed(e.t, e.in(e.msg("CfToInbox"), "Work"))
	r := e.ref("CfToInbox")
	e.checkKept(one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, "INBOX")), r)
	if got := e.labels("CfToInbox"); !hasFold(got, "Work") || !hasFold(got, `\Inbox`) {
		e.t.Errorf("labels = %q, want Work and \\Inbox", got)
	}
}

func moveToBinGmail(e *env) {
	e.seed(e.t, e.msg("CfToTrash"), e.msg("CfToSpam"))
	for token, dest := range map[string]string{"CfToTrash": GmailTrash, "CfToSpam": GmailSpam} {
		r := e.ref(token)
		e.checkPlaced(one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, dest)), r, dest, token)
		e.wantWhere(token, dest)
	}
}

func moveFromTrashGmail(e *env) {
	e.seed(e.t, e.in(e.msg("CfFromTrash"), GmailTrash), e.in(e.msg("CfFromTrashWork"), "Work"))
	r := e.ref("CfFromTrash", "trash")
	got := one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, "Work"))
	e.checkPlaced(got, r, GmailAllMail, "CfFromTrash")
	e.wantWhere("CfFromTrash", GmailAllMail)
	if labels := e.labels("CfFromTrash"); !hasFold(labels, "Work") || hasFold(labels, `\Inbox`) {
		e.t.Errorf("labels = %q, want Work and not \\Inbox", labels)
	}
}

// moveBackGmail: a move out of Spam to INBOX goes back to All Mail and
// gains \Inbox; a move out of Trash to All Mail goes back and gains nothing.
func moveBackGmail(e *env) {
	e.seed(e.t, e.in(e.msg("CfBackSpam"), GmailSpam), e.in(e.msg("CfBackTrash"), GmailTrash))
	sp, tr := e.ref("CfBackSpam", "spam"), e.ref("CfBackTrash", "trash")
	e.checkPlaced(one(e.t, e.s.Move(e.ctx, []ref.Ref{sp}, "INBOX")), sp, GmailAllMail, "CfBackSpam")
	e.wantWhere("CfBackSpam", GmailAllMail)
	if labels := e.labels("CfBackSpam"); !hasFold(labels, `\Inbox`) {
		e.t.Errorf("labels = %q, want \\Inbox", labels)
	}
	e.stale(sp)
	e.checkPlaced(one(e.t, e.s.Move(e.ctx, []ref.Ref{tr}, GmailAllMail)), tr, GmailAllMail, "CfBackTrash")
	e.wantWhere("CfBackTrash", GmailAllMail)
	if labels := e.labels("CfBackTrash"); hasFold(labels, `\Inbox`) {
		e.t.Errorf("labels = %q, want no \\Inbox: a move to All Mail adds no label", labels)
	}
	e.stale(tr)
}

func copyStandard(e *env) {
	e.seed(e.t, e.msg("CfCopy"))
	r := e.ref("CfCopy")
	e.checkPlaced(one(e.t, e.s.Copy(e.ctx, []ref.Ref{r}, "Archive")), r, "Archive", "CfCopy")
	e.readable(r, "CfCopy")
	e.wantWhere("CfCopy", "Archive", "INBOX")
}

func copyIntoOwnFolder(e *env) {
	e.seed(e.t, e.msg("CfCopyOwn"))
	r := e.ref("CfCopyOwn")
	e.checkPlaced(one(e.t, e.s.Copy(e.ctx, []ref.Ref{r}, "INBOX")), r, "INBOX", "CfCopyOwn")
	e.readable(r, "CfCopyOwn")
	e.wantWhere("CfCopyOwn", "INBOX", "INBOX")
}

func copyToLabelGmail(e *env) {
	e.gmailWithWork("CfCopyLabel")
	r := e.ref("CfCopyLabel")
	e.checkKept(one(e.t, e.s.Copy(e.ctx, []ref.Ref{r}, "Work")), r)
	if got := e.labels("CfCopyLabel"); !hasFold(got, "Work") || !hasFold(got, `\Inbox`) {
		e.t.Errorf("labels = %q, want Work and \\Inbox", got)
	}
	e.checkKept(one(e.t, e.s.Copy(e.ctx, []ref.Ref{r}, GmailAllMail)), r)
	e.wantWhere("CfCopyLabel", GmailAllMail)
}

func copyToBinGmail(e *env) {
	e.seed(e.t, e.msg("CfCopyTrash"), e.msg("CfCopySpam"))
	for token, dest := range map[string]string{"CfCopyTrash": GmailTrash, "CfCopySpam": GmailSpam} {
		r := e.ref(token)
		e.checkPlaced(one(e.t, e.s.Copy(e.ctx, []ref.Ref{r}, dest)), r, dest, token)
		e.wantWhere(token, dest)
	}
}

// copyFromBinToAllMailGmail: a message in Trash or Spam is not in All Mail,
// and Gmail keeps one copy of a message, so copying it there is refused per
// ref, while a ref in All Mail in the same batch is a no-op success.
func copyFromBinToAllMailGmail(e *env) {
	e.seed(e.t, e.in(e.msg("CfCopyBackTrash"), GmailTrash), e.msg("CfCopyBackAll"), e.in(e.msg("CfCopyBackSpam"), GmailSpam))
	tr, all, sp := e.ref("CfCopyBackTrash", "trash"), e.ref("CfCopyBackAll"), e.ref("CfCopyBackSpam", "spam")
	res := e.s.Copy(e.ctx, []ref.Ref{tr, all, sp}, GmailAllMail)
	if len(res) != 3 {
		e.t.Fatalf("Copy gave %d results for 3 refs", len(res))
	}
	wantErr(e.t, "Copy from Trash to All Mail", res[0].Err, mailstore.ErrUnsupported)
	e.checkKept(res[1], all)
	wantErr(e.t, "Copy from Spam to All Mail", res[2].Err, mailstore.ErrUnsupported)
	e.wantWhere("CfCopyBackTrash", GmailTrash)
	e.wantWhere("CfCopyBackAll", GmailAllMail)
	e.wantWhere("CfCopyBackSpam", GmailSpam)
	e.readable(tr, "CfCopyBackTrash")
	e.readable(sp, "CfCopyBackSpam")
}

func archive(e *env) {
	if e.gmail {
		e.gmailWithWork("CfArchive")
		r := e.ref("CfArchive")
		if err := one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, "Work")).Err; err != nil {
			e.t.Fatal(err)
		}
		if err := one(e.t, e.s.Move(e.ctx, []ref.Ref{r}, "INBOX")).Err; err != nil {
			e.t.Fatal(err)
		}
		e.checkKept(one(e.t, e.s.Archive(e.ctx, []ref.Ref{r})), r)
		if got := e.labels("CfArchive"); hasFold(got, `\Inbox`) || !hasFold(got, "Work") {
			e.t.Errorf("labels = %q, want Work kept and \\Inbox gone", got)
		}
		e.checkKept(one(e.t, e.s.Archive(e.ctx, []ref.Ref{r})), r)
		return
	}
	e.seed(e.t, e.msg("CfArchive"), e.in(e.msg("CfArchived"), "Archive"))
	r, done := e.ref("CfArchive"), e.ref("CfArchived")
	res := e.s.Archive(e.ctx, []ref.Ref{r, done})
	if len(res) != 2 {
		e.t.Fatalf("Archive gave %d results", len(res))
	}
	e.checkPlaced(res[0], r, "Archive", "CfArchive")
	e.checkKept(res[1], done)
	e.wantWhere("CfArchive", "Archive")
}

// archiveConfigured: with archive_folder set, Archive moves there and not
// to the folder with the \Archive role. Google accounts archive by label
// whatever it says.
func archiveConfigured(e *env) {
	const saved = "Cf Saved"
	e.open(Config{ArchiveFolder: saved})
	if e.gmail {
		e.seed(e.t, e.msg("CfArchiveSet"))
		r := e.ref("CfArchiveSet")
		e.checkKept(one(e.t, e.s.Archive(e.ctx, []ref.Ref{r})), r)
		if got := e.labels("CfArchiveSet"); hasFold(got, `\Inbox`) {
			e.t.Errorf("labels = %q, want \\Inbox gone", got)
		}
		e.wantWhere("CfArchiveSet", GmailAllMail)
		return
	}
	e.mustCreate(saved)
	e.seed(e.t, e.msg("CfArchiveSet"), e.in(e.msg("CfArchiveSetDone"), saved))
	r, done := e.ref("CfArchiveSet"), e.ref("CfArchiveSetDone", saved)
	res := e.s.Archive(e.ctx, []ref.Ref{r, done})
	if len(res) != 2 {
		e.t.Fatalf("Archive gave %d results for 2 refs", len(res))
	}
	e.checkPlaced(res[0], r, saved, "CfArchiveSet")
	e.checkKept(res[1], done)
	e.wantWhere("CfArchiveSet", saved)
	e.wantWhere("CfArchiveSetDone", saved)
}

func trash(e *env) {
	e.seed(e.t, e.msg("CfTrash"), e.in(e.msg("CfTrashed"), e.trash()))
	r, done := e.ref("CfTrash"), e.ref("CfTrashed", "trash")
	res := e.s.Trash(e.ctx, []ref.Ref{r, done})
	if len(res) != 2 {
		e.t.Fatalf("Trash gave %d results", len(res))
	}
	e.checkPlaced(res[0], r, e.trash(), "CfTrash")
	e.checkKept(res[1], done)
	e.wantWhere("CfTrash", e.trash())
	e.wantWhere("CfTrashed", e.trash())
}

func expunge(e *env) {
	keep := e.msg("CfExpungeKeep")
	if !e.gmail {
		// UID EXPUNGE removes only the given UIDs, not other mail marked \Deleted.
		keep.Flags = []string{`\Deleted`}
	}
	e.seed(e.t, e.msg("CfExpunge"), keep, e.in(e.msg("CfExpungeTrash"), e.trash()), e.in(e.msg("CfExpungeTrashKeep"), e.trash()))
	r, t := e.ref("CfExpunge"), e.ref("CfExpungeTrash", "trash")
	res := e.s.Expunge(e.ctx, []ref.Ref{r, t})
	if len(res) != 2 {
		e.t.Fatalf("Expunge gave %d results for 2 refs", len(res))
	}
	for i, got := range res {
		if got != (mailstore.RefResult{}) {
			e.t.Errorf("Expunge result %d = %+v, want a zero ref", i, got)
		}
	}
	e.stale(r)
	e.wantWhere("CfExpunge")
	e.wantWhere("CfExpungeTrash")
	e.wantWhere("CfExpungeKeep", e.home())
	e.wantWhere("CfExpungeTrashKeep", e.trash())
	wantErr(e.t, "Expunge of an expunged ref", one(e.t, e.s.Expunge(e.ctx, []ref.Ref{r})).Err, mailstore.ErrStaleRef)
}

func storeFlags(e *env) {
	e.seed(e.t, e.msg("CfFlags"))
	r := e.ref("CfFlags")
	e.checkKept(one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{r}, []string{`\Seen`, `\Flagged`, "$Conformance"}, nil)), r)
	got := e.only("", "CfFlags")
	for _, f := range []string{`\Seen`, `\Flagged`, "$Conformance"} {
		if !hasFold(got.Flags, f) {
			e.t.Errorf("flags = %q, want %s", got.Flags, f)
		}
	}
	if e.gmail && !hasFold(got.Labels, `\Starred`) {
		e.t.Errorf("labels = %q, want \\Starred: \\Flagged is the star", got.Labels)
	}
	e.checkKept(one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{r}, nil, []string{`\Flagged`})), r)
	got = e.only("", "CfFlags")
	if hasFold(got.Flags, `\Flagged`) || !hasFold(got.Flags, `\Seen`) {
		e.t.Errorf("flags = %q, want \\Seen kept and \\Flagged gone", got.Flags)
	}
	if e.gmail && hasFold(got.Labels, `\Starred`) {
		e.t.Errorf("labels = %q, want \\Starred gone with \\Flagged", got.Labels)
	}
}

func storeFlagsBad(e *env) {
	e.seed(e.t, e.msg("CfFlagsBad"))
	r := e.ref("CfFlagsBad")
	for _, f := range []string{`\Recent`, "two words", ""} {
		wantErr(e.t, fmt.Sprintf("StoreFlags(%q)", f), one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{r}, []string{f}, nil)).Err, mailstore.ErrUnsupported)
	}
	// Dovecot shows \Recent to the first session that sees a message.
	if got := slices.DeleteFunc(e.only("", "CfFlagsBad").Flags, func(f string) bool { return f == `\Recent` }); len(got) != 0 {
		e.t.Errorf("flags = %q after refused stores", got)
	}
}

func storeLabelsStandard(e *env) {
	e.seed(e.t, e.msg("CfLabelsStd"))
	r := e.ref("CfLabelsStd")
	res := e.s.StoreLabels(e.ctx, []ref.Ref{r, r}, []string{"Work"}, nil)
	if len(res) != 2 {
		e.t.Fatalf("StoreLabels gave %d results for 2 refs", len(res))
	}
	for i, got := range res {
		wantErr(e.t, fmt.Sprintf("StoreLabels result %d", i), got.Err, mailstore.ErrUnsupported)
	}
}

func storeLabelsGmail(e *env) {
	e.gmailWithWork("CfLabels")
	r := e.ref("CfLabels")
	add := []string{"work", "Conformance New", `\important`}
	e.checkKept(one(e.t, e.s.StoreLabels(e.ctx, []ref.Ref{r}, add, []string{`\Inbox`})), r)
	got := e.labels("CfLabels")
	for _, l := range []string{"Work", "Conformance New", `\Important`} {
		if !slices.Contains(got, l) {
			e.t.Errorf("labels = %q, want %q", got, l)
		}
	}
	if hasFold(got, `\Inbox`) || slices.Contains(got, "work") {
		e.t.Errorf("labels = %q, want \\Inbox gone and work given as Work", got)
	}
	if !slices.Contains(e.folders(), "Conformance New") {
		e.t.Errorf("folders = %q, want the new label Conformance New", e.folders())
	}
	if got := e.matches("Conformance New", "CfLabels"); len(got) != 1 {
		e.t.Errorf("in:\"Conformance New\" found %v", refsOf(got))
	}
	e.checkKept(one(e.t, e.s.StoreLabels(e.ctx, []ref.Ref{r}, []string{"INBOX"}, []string{"Conformance New"})), r)
	if got := e.labels("CfLabels"); !slices.Contains(got, `\Inbox`) || hasFold(got, "Conformance New") {
		e.t.Errorf("labels = %q, want INBOX given as \\Inbox and Conformance New gone", got)
	}
}

func storeLabelsRefused(e *env) {
	e.seed(e.t, e.msg("CfLabelsBad"))
	r := e.ref("CfLabelsBad")
	for _, l := range []string{GmailAllMail, GmailTrash, "[gmail]/spam", "[Gmail]/Conformance", ""} {
		wantErr(e.t, fmt.Sprintf("StoreLabels(%q)", l), one(e.t, e.s.StoreLabels(e.ctx, []ref.Ref{r}, []string{l}, nil)).Err, mailstore.ErrUnsupported)
	}
	wantErr(e.t, `StoreLabels(\Bogus)`, one(e.t, e.s.StoreLabels(e.ctx, []ref.Ref{r}, []string{`\Bogus`}, nil)).Err, mailstore.ErrNotFound)
	if got := e.labels("CfLabelsBad"); !slices.Equal(got, []string{`\Inbox`}) {
		e.t.Errorf("labels = %q after refused edits, want only \\Inbox", got)
	}
}

func createFolder(e *env) {
	e.mustCreate("Cf/Sub/Leaf")
	got := e.folders()
	for _, f := range []string{"Cf", "Cf/Sub", "Cf/Sub/Leaf"} {
		if !slices.Contains(got, f) {
			e.t.Errorf("folders = %q, want %q", got, f)
		}
	}
	for _, name := range []string{"Cf/Sub/Leaf", "Cf/Sub", "inbox", pick(e.gmail, "cf/sub", "Trash")} {
		wantErr(e.t, fmt.Sprintf("CreateFolder(%q)", name), errOf(e.s.CreateFolder(e.ctx, name)), mailstore.ErrExists)
	}
	for _, name := range []string{"", "Cf//Gap", "/Cf"} {
		wantErr(e.t, fmt.Sprintf("CreateFolder(%q)", name), errOf(e.s.CreateFolder(e.ctx, name)), mailstore.ErrRefused)
	}
	if e.gmail {
		wantErr(e.t, "CreateFolder of a Gmail folder", errOf(e.s.CreateFolder(e.ctx, "[Gmail]/Conformance")), mailstore.ErrRefused)
		wantErr(e.t, "CreateFolder of a label in another case", errOf(e.s.CreateFolder(e.ctx, "CF/sub")), mailstore.ErrExists)
	}
	if got := e.folders(); slices.Contains(got, "Cf/Gap") || slices.Contains(got, "[Gmail]/Conformance") {
		e.t.Errorf("folders = %q after refused creates", got)
	}
}

func renameFolder(e *env) {
	e.mustCreate("Cf/Sub")
	e.seed(e.t, e.in(e.msg("CfRename"), "Cf/Sub"))
	before := e.ref("CfRename", pick(e.gmail, "", "Cf/Sub"))
	if got, err := e.s.RenameFolder(e.ctx, "Cf", "Moved/Cf"); err != nil || got != "Moved/Cf" {
		e.t.Fatalf("RenameFolder = %q, %v; want Moved/Cf", got, err)
	}
	got := e.folders()
	for _, f := range []string{"Moved", "Moved/Cf", "Moved/Cf/Sub"} {
		if !slices.Contains(got, f) {
			e.t.Errorf("folders = %q, want %q", got, f)
		}
	}
	if slices.Contains(got, "Cf") || slices.Contains(got, "Cf/Sub") {
		e.t.Errorf("folders = %q, want Cf and Cf/Sub gone", got)
	}
	if e.gmail {
		e.readable(before, "CfRename")
		if got := e.labels("CfRename"); !slices.Contains(got, "Moved/Cf/Sub") || slices.Contains(got, "Cf/Sub") {
			e.t.Errorf("labels = %q, want Cf/Sub renamed to Moved/Cf/Sub", got)
		}
		return
	}
	e.wantWhere("CfRename", "Moved/Cf/Sub")
	e.readable(e.ref("CfRename", "Moved/Cf/Sub"), "CfRename")
}

func renameRefused(e *env) {
	e.mustCreate("Cf/Sub")
	e.mustCreate("Other")
	for _, c := range []struct {
		from, to string
		want     error
	}{
		{"INBOX", "Conformance Inbox", mailstore.ErrRefused},
		{e.trash(), "Conformance Bin", mailstore.ErrRefused},
		{"Conformance Missing", "Conformance Found", mailstore.ErrNotFound},
		{"Cf", "Other", mailstore.ErrExists},
		{"Cf", "Cf/Sub/Cf", mailstore.ErrRefused},
		{"Cf", "Bad//Name", mailstore.ErrRefused},
	} {
		wantErr(e.t, fmt.Sprintf("RenameFolder(%q, %q)", c.from, c.to), errOf(e.s.RenameFolder(e.ctx, c.from, c.to)), c.want)
	}
	if e.gmail {
		wantErr(e.t, "RenameFolder into Gmail's folders", errOf(e.s.RenameFolder(e.ctx, "Other", "[Gmail]/Other")), mailstore.ErrRefused)
	}
	got := e.folders()
	for _, f := range []string{"Cf", "Cf/Sub", "Other", "INBOX", e.trash()} {
		if !slices.Contains(got, f) {
			e.t.Errorf("folders = %q after refused renames, want %q", got, f)
		}
	}
}

func deleteFolder(e *env) {
	e.mustCreate("Cf/Sub")
	e.seed(e.t, e.in(e.msg("CfDelete"), "Cf/Sub"))
	for _, c := range []struct {
		name string
		want error
	}{
		{"INBOX", mailstore.ErrRefused},
		{e.trash(), mailstore.ErrRefused},
		{e.spam(), mailstore.ErrRefused},
		{"Conformance Missing", mailstore.ErrNotFound},
		{"Cf", mailstore.ErrRefused},     // it has a child folder
		{"Cf/Sub", mailstore.ErrRefused}, // it holds mail
	} {
		wantErr(e.t, fmt.Sprintf("DeleteFolder(%q, false)", c.name), e.s.DeleteFolder(e.ctx, c.name, false), c.want)
	}
	wantErr(e.t, "DeleteFolder(INBOX, true)", e.s.DeleteFolder(e.ctx, "INBOX", true), mailstore.ErrRefused)
	wantErr(e.t, "DeleteFolder(Cf, true) with a child", e.s.DeleteFolder(e.ctx, "Cf", true), mailstore.ErrRefused)
	if err := e.s.DeleteFolder(e.ctx, "Cf/Sub", true); err != nil {
		e.t.Fatalf("DeleteFolder(Cf/Sub, true): %v", err)
	}
	name := "Cf"
	if e.gmail {
		name = "cF" // Gmail matches label names ignoring case
	}
	if err := e.s.DeleteFolder(e.ctx, name, false); err != nil {
		e.t.Fatalf("DeleteFolder(%q, false) once empty: %v", name, err)
	}
	if got := e.folders(); slices.Contains(got, "Cf") || slices.Contains(got, "Cf/Sub") {
		e.t.Errorf("folders = %q, want Cf and Cf/Sub gone", got)
	}
	if e.gmail {
		// A deleted label leaves its messages in All Mail.
		e.wantWhere("CfDelete", GmailAllMail)
		if got := e.labels("CfDelete"); hasFold(got, "Cf/Sub") {
			e.t.Errorf("labels = %q, want Cf/Sub gone", got)
		}
		return
	}
	e.wantWhere("CfDelete")
}

// findByMessageID: the refs are those a search gives, so on Google they
// carry the message's X-GM-MSGID, which SEARCH alone does not give.
// folderNamespace: CreateFolder and RenameFolder return the name the server
// gave the folder, which is how the account lists it and how later calls
// name it. On the harness's own account that is the name as given. Under a
// namespace prefix such as "INBOX.", with "." between levels, it is written
// the server's way, as Hostinger needs.
func folderNamespace(e *env) {
	check := func(ns Namespace) {
		e.t.Helper()
		for _, name := range []string{"CfNs/Sub", ns.serverName("CfNsKept")} {
			want := ns.serverName(name)
			got, err := e.s.CreateFolder(e.ctx, name)
			if err != nil || got != want {
				e.t.Fatalf("CreateFolder(%q) = %q, %v; want %q", name, got, err, want)
			}
			if !slices.Contains(e.folders(), want) {
				e.t.Errorf("folders = %q, want %q", e.folders(), want)
			}
		}
		wantErr(e.t, "CreateFolder of a name the account has under the prefix", errOf(e.s.CreateFolder(e.ctx, "CfNsKept")), mailstore.ErrExists)
		from, to := ns.serverName("CfNsKept"), ns.serverName("CfNsMoved/Here")
		got, err := e.s.RenameFolder(e.ctx, from, "CfNsMoved/Here")
		if err != nil || got != to {
			e.t.Fatalf("RenameFolder(%q, CfNsMoved/Here) = %q, %v; want %q", from, got, err, to)
		}
		if list := e.folders(); !slices.Contains(list, to) || slices.Contains(list, from) {
			e.t.Errorf("folders = %q, want %q renamed to %q", list, from, to)
		}
		if err := e.s.DeleteFolder(e.ctx, got, false); err != nil {
			e.t.Errorf("DeleteFolder(%q), the name RenameFolder gave: %v", got, err)
		}
	}
	check(Namespace{})
	if e.namespaces && !e.gmail {
		e.open(Config{Namespace: Namespace{Prefix: "INBOX.", Delim: "."}})
		check(Namespace{Prefix: "INBOX.", Delim: "."})
	}
}

func findByMessageID(e *env) {
	e.seed(e.t, e.msg("CfFind"), e.in(e.msg("CfFindTrash"), e.trash()))
	r := e.ref("CfFind")
	id := e.msg("CfFind").MessageID
	for _, form := range []string{id, "<" + id + ">", " <" + id + "> "} {
		got, err := e.s.FindByMessageID(e.ctx, e.home(), form)
		if err != nil || len(got) != 1 || got[0] != r {
			e.t.Errorf("FindByMessageID(%s, %q) = %v, %v; want %v", e.home(), form, got, err, r)
		}
	}
	for _, other := range []string{"", "nobody@conformance.test"} {
		if got, err := e.s.FindByMessageID(e.ctx, e.home(), other); err != nil || len(got) != 0 {
			e.t.Errorf("FindByMessageID(%q) = %v, %v; want nothing", other, got, err)
		}
	}
	if got, err := e.s.FindByMessageID(e.ctx, e.home(), e.msg("CfFindTrash").MessageID); err != nil || len(got) != 0 {
		e.t.Errorf("FindByMessageID of a message in Trash, searching %s = %v, %v; want nothing", e.home(), got, err)
	}
	trashed := e.ref("CfFindTrash", "trash")
	if got, err := e.s.FindByMessageID(e.ctx, e.trash(), e.msg("CfFindTrash").MessageID); err != nil || len(got) != 1 || got[0] != trashed {
		e.t.Errorf("FindByMessageID in Trash = %v, %v; want [%v]", got, err, trashed)
	}
	if e.gmail {
		got, err := e.s.FindByMessageID(e.ctx, "INBOX", id)
		if err != nil || len(got) != 1 || got[0].Folder != "INBOX" || got[0].GmailMsgID != r.GmailMsgID || r.GmailMsgID == 0 {
			e.t.Fatalf("FindByMessageID(INBOX) = %v, %v; want a ref in INBOX with X-GM-MSGID %d", got, err, r.GmailMsgID)
		}
		e.readable(got[0], "CfFind")
	}
	_, err := e.s.FindByMessageID(e.ctx, "Conformance Missing", id)
	wantErr(e.t, "FindByMessageID in a missing folder", err, mailstore.ErrNotFound)
}

func listFolders(e *env) {
	read := e.msg("CfListRead")
	read.Flags = []string{`\Seen`}
	e.seed(e.t, e.msg("CfListUnread"), read)
	e.mustCreate("aaa conformance")
	e.mustCreate("ZZZ conformance")
	list, err := e.s.ListFolders(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(list) == 0 || list[0].Name != "INBOX" {
		e.t.Fatalf("folders = %v, want INBOX first", list)
	}
	for i := 2; i < len(list); i++ {
		if a, b := strings.ToLower(list[i-1].Name), strings.ToLower(list[i].Name); cmp.Compare(a, b) > 0 {
			e.t.Errorf("folders %q before %q, want them by name ignoring case", list[i-1].Name, list[i].Name)
		}
	}
	want := map[string]mailstore.Role{"INBOX": mailstore.RoleInbox, "aaa conformance": mailstore.RoleNone, "ZZZ conformance": mailstore.RoleNone}
	if e.gmail {
		want[GmailAllMail], want[GmailSent], want[GmailTrash], want[GmailSpam] = mailstore.RoleAll, mailstore.RoleSent, mailstore.RoleTrash, mailstore.RoleSpam
	} else {
		want["Sent"], want["Drafts"], want["Archive"], want["Trash"], want["Junk"] = mailstore.RoleSent, mailstore.RoleDrafts, mailstore.RoleArchive, mailstore.RoleTrash, mailstore.RoleSpam
	}
	for _, f := range list {
		role, ok := want[f.Name]
		if !ok {
			continue
		}
		delete(want, f.Name)
		if f.Role != role || !f.Selectable {
			e.t.Errorf("%s: role %q, selectable %v; want %q, selectable", f.Name, f.Role, f.Selectable, role)
		}
		if (f.Name == "INBOX" || f.Role == mailstore.RoleAll) && (f.Messages != 2 || f.Unseen != 1) {
			e.t.Errorf("%s counts = %d/%d, want 2/1", f.Name, f.Messages, f.Unseen)
		}
		if f.Role == mailstore.RoleTrash && f.Messages != 0 {
			e.t.Errorf("%s holds %d messages, want 0", f.Name, f.Messages)
		}
	}
	if len(want) > 0 {
		e.t.Errorf("folders %v lack %v", list, want)
	}
}

func searchBasics(e *env) {
	other := pick(e.gmail, "Work", "Archive")
	from := e.msg("CfSearchOther")
	from.From = "billing@conformance.test"
	e.seed(e.t, e.msg("CfSearchHome"), e.in(from, other), e.in(e.msg("CfSearchBin"), e.trash()))
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", []string{e.home(), e.archive()}},
		{"trash", []string{e.trash()}},
		{"anywhere", []string{e.home(), e.archive(), e.trash()}},
		{other, []string{e.archive()}},
	} {
		var got []string
		for _, s := range e.matches(c.in, "CfSearch") {
			got = append(got, s.Ref.Folder)
		}
		slices.Sort(got)
		slices.Sort(c.want)
		if !slices.Equal(got, c.want) {
			e.t.Errorf("in:%q found messages in %q, want %q", c.in, got, c.want)
		}
	}
	var total uint32
	for _, r := range e.search("", "CfSearch", true) {
		total += r.Count
		if len(r.UIDs) != 0 {
			e.t.Errorf("a count-only search gave UIDs in %s", r.Source.Folder)
		}
		if r.Source.UIDValidity == 0 {
			e.t.Errorf("source %s has no UIDVALIDITY", r.Source.Folder)
		}
	}
	if total != 2 {
		e.t.Errorf("count = %d, want 2", total)
	}
	n, err := query.Parse("from:billing@conformance.test")
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.s.Search(e.ctx, mailstore.SearchRequest{Query: n})
	if err != nil {
		e.t.Fatal(err)
	}
	var hits int
	for _, r := range res {
		hits += len(r.UIDs)
	}
	if hits != 1 {
		e.t.Errorf("from: found %d messages, want 1", hits)
	}
	_, err = e.s.Search(e.ctx, mailstore.SearchRequest{In: "Conformance Missing"})
	if !e.gmail {
		wantErr(e.t, "in: a missing folder", err, mailstore.ErrNotFound)
	}
}

func fetchSummaries(e *env) {
	e.seed(e.t, e.msg("CfSumA"), e.msg("CfSumB"))
	a, b := e.ref("CfSumA"), e.ref("CfSumB")
	src := mailstore.Source{Folder: a.Folder, UIDValidity: a.UIDValidity}
	gone := max(a.UID, b.UID) + 1000
	sums, err := e.s.FetchSummaries(e.ctx, src, []uint32{b.UID, gone, a.UID}, mailstore.SummaryOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	if len(sums) != 2 || sums[0].Ref != b || sums[1].Ref != a {
		e.t.Fatalf("FetchSummaries = %v, want B then A, the gone UID left out", refsOf(sums))
	}
	s := sums[1]
	if s.Subject != "CfSumA" || len(s.From) != 1 || s.From[0].Email != "alice@example.com" || !s.Date.Equal(day(20)) || s.HasAttachment || s.ThreadID == "" {
		e.t.Errorf("summary = %+v", s)
	}
	if e.gmail && !hasFold(s.Labels, `\Inbox`) {
		e.t.Errorf("labels = %q, want \\Inbox", s.Labels)
	}
	stale := src
	stale.UIDValidity++
	_, err = e.s.FetchSummaries(e.ctx, stale, []uint32{a.UID}, mailstore.SummaryOptions{})
	wantErr(e.t, "FetchSummaries with another UIDVALIDITY", err, mailstore.ErrStaleRef)
}

// richMessage is a reply with two attachments.
func (e *env) richMessage(token string) Message {
	m := e.msg(token)
	m.InReplyTo = "root@conformance.test"
	m.References = []string{"older@conformance.test", "root@conformance.test"}
	m.Attachments = []Attachment{
		{Filename: "invoice.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 conformance invoice")},
		{Filename: "notes.txt", ContentType: "text/plain", Data: []byte("conformance notes")},
	}
	return m
}

func fetchMessages(e *env) {
	want := e.richMessage("CfFetch")
	e.seed(e.t, want, e.msg("CfFetchPlain"))
	r, plain := e.ref("CfFetch"), e.ref("CfFetchPlain")
	// A write first leaves the folder open read-write; the reads after it
	// still must not set \Seen.
	e.checkKept(one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{plain}, []string{"$Conformance"}, nil)), plain)
	res := e.s.FetchMessages(e.ctx, []ref.Ref{r, foreign(r), plain, r})
	if len(res) != 4 {
		e.t.Fatalf("FetchMessages gave %d results for 4 refs", len(res))
	}
	wantErr(e.t, "FetchMessages of another account's ref", res[1].Err, mailstore.ErrNotFound)
	if res[2].Err != nil || res[2].Message.Summary.Subject != "CfFetchPlain" {
		e.t.Errorf("plain message = %+v", res[2])
	}
	for _, i := range []int{0, 3} {
		if res[i].Err != nil {
			e.t.Fatalf("FetchMessages result %d: %v", i, res[i].Err)
		}
	}
	m := res[0].Message
	if m.Summary.Ref != r || m.Summary.Subject != "CfFetch" || !m.Summary.HasAttachment || bare(m.MessageID) != want.MessageID {
		e.t.Errorf("message = ref %v subject %q attachment %v id %q", m.Summary.Ref, m.Summary.Subject, m.Summary.HasAttachment, m.MessageID)
	}
	if got := mailstore.MessageIDs(m.InReplyTo); !slices.Equal(got, []string{want.InReplyTo}) {
		e.t.Errorf("In-Reply-To = %q", m.InReplyTo)
	}
	if got := mailstore.MessageIDs(m.References); !slices.Equal(got, want.References) {
		e.t.Errorf("References = %q, want %q", m.References, want.References)
	}
	if len(m.Attachments) != len(want.Attachments) {
		e.t.Fatalf("attachments = %+v", m.Attachments)
	}
	for i, a := range want.Attachments {
		wantA := mailstore.Attachment{Part: AttachmentPart(i), Filename: a.Filename, ContentType: a.ContentType, Size: int64(base64.StdEncoding.EncodedLen(len(a.Data)))}
		if m.Attachments[i] != wantA {
			e.t.Errorf("attachment %d = %+v, want %+v", i, m.Attachments[i], wantA)
		}
	}
	if !bytes.Contains(m.Raw, []byte(want.Body)) || bytes.Contains(m.Raw, []byte(base64.StdEncoding.EncodeToString(want.Attachments[0].Data))) {
		e.t.Errorf("Raw = %q, want the text without attachment bodies", m.Raw)
	}
	if m.TextClipped {
		e.t.Error("TextClipped on a short text")
	}
	if got := res[3].Message; got.Summary.Ref != r {
		e.t.Errorf("repeated ref read as %v", got.Summary.Ref)
	}
	e.unread("CfFetch")
	e.unread("CfFetchPlain")
}

func streamPart(e *env) {
	want := e.richMessage("CfStream")
	e.seed(e.t, want)
	r := e.ref("CfStream")
	// A write first leaves the folder open read-write; the reads after it
	// still must not set \Seen.
	e.checkKept(one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{r}, []string{"$Conformance"}, nil)), r)
	read := func(part string) (mailstore.PartInfo, []byte, error) {
		info, rc, err := e.s.StreamPart(e.ctx, r, part)
		if err != nil {
			return info, nil, err
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		return info, data, err
	}
	_, whole, err := read("")
	if err != nil {
		e.t.Fatal(err)
	}
	if !bytes.Contains(whole, []byte(want.Body)) || !bytes.Contains(whole, []byte("Subject: CfStream")) {
		e.t.Errorf("whole message = %q", whole)
	}
	e.unread("CfStream")
	for i, a := range want.Attachments {
		info, data, err := read(AttachmentPart(i))
		if err != nil {
			e.t.Errorf("StreamPart(%s): %v", AttachmentPart(i), err)
			continue
		}
		if !bytes.Equal(data, a.Data) || info.Filename != a.Filename || info.ContentType != a.ContentType {
			e.t.Errorf("part %s = %+v %q, want %s %s %q", AttachmentPart(i), info, data, a.Filename, a.ContentType, a.Data)
		}
	}
	_, _, err = read("9")
	wantErr(e.t, "StreamPart of a missing part", err, mailstore.ErrNotFound)
	e.unread("CfStream")
	if got := one(e.t, e.s.Expunge(e.ctx, []ref.Ref{r})); got.Err != nil {
		e.t.Fatal(got.Err)
	}
	_, _, err = read("")
	wantErr(e.t, "StreamPart of an expunged message", err, mailstore.ErrStaleRef)
}

func thread(e *env) {
	root := e.msg("CfThreadRoot")
	root.MessageID = "thread-root@conformance.test"
	reply := e.in(e.msg("CfThreadReply"), pick(e.gmail, GmailSent, "Sent"))
	if e.gmail {
		reply.Labels = []string{`\Sent`}
	}
	reply.Date, reply.InReplyTo, reply.References = day(21), root.MessageID, []string{root.MessageID}
	binned := e.in(e.msg("CfThreadBinned"), e.trash())
	binned.Date, binned.InReplyTo, binned.References = day(22), root.MessageID, []string{root.MessageID}
	e.seed(e.t, root, reply, binned, e.msg("CfThreadOther"))
	rootRef, replyRef := e.ref("CfThreadRoot"), e.ref("CfThreadReply")
	want := []ref.Ref{rootRef, replyRef}
	sortRefs(want)
	for _, seed := range []ref.Ref{rootRef, replyRef} {
		got, err := e.s.Thread(e.ctx, seed)
		if err != nil {
			e.t.Fatalf("Thread(%v): %v", seed, err)
		}
		sortRefs(got)
		if !slices.Equal(got, want) {
			e.t.Errorf("Thread(%v) = %v, want %v", seed, got, want)
		}
	}
	single, err := e.s.Thread(e.ctx, e.ref("CfThreadOther"))
	if err != nil || len(single) != 1 {
		e.t.Errorf("Thread of a lone message = %v, %v", single, err)
	}
}

func fetchHeaders(e *env) {
	hf, ok := e.s.(mailstore.HeaderFetcher)
	if !ok {
		e.t.Fatalf("%T does not implement mailstore.HeaderFetcher", e.s)
	}
	want := e.richMessage("CfHead")
	want.Cc, want.Bcc = "carol@conformance.test", "dave@conformance.test"
	e.seed(e.t, want, e.msg("CfHeadPlain"))
	r, plain := e.ref("CfHead"), e.ref("CfHeadPlain")
	// A write first leaves the folder open read-write; the reads after it
	// still must not set \Seen.
	e.checkKept(one(e.t, e.s.StoreFlags(e.ctx, []ref.Ref{plain}, []string{"$Conformance"}, nil)), plain)
	src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
	gone := max(r.UID, plain.UID) + 1000
	hs, err := hf.FetchHeaders(e.ctx, src, []uint32{plain.UID, gone, r.UID})
	if err != nil {
		e.t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Summary.Ref != plain || hs[1].Summary.Ref != r {
		e.t.Fatalf("FetchHeaders gave %d headers, want CfHeadPlain then CfHead with the gone UID left out", len(hs))
	}
	sums, err := e.s.FetchSummaries(e.ctx, src, []uint32{plain.UID, r.UID}, mailstore.SummaryOptions{})
	if err != nil || len(sums) != 2 {
		e.t.Fatalf("FetchSummaries = %v, %v", refsOf(sums), err)
	}
	for i, h := range hs {
		if !sameSummary(h.Summary, sums[i]) {
			e.t.Errorf("header %d summary = %+v, want FetchSummaries' %+v", i, h.Summary, sums[i])
		}
		if h.Summary.Preview != nil || h.Summary.Sample != nil {
			e.t.Errorf("header %d carries a snippet", i)
		}
	}
	h := hs[1]
	if !slices.Equal(h.Cc, []mailstore.Address{{Email: want.Cc}}) || !slices.Equal(h.Bcc, []mailstore.Address{{Email: want.Bcc}}) {
		e.t.Errorf("Cc = %v, Bcc = %v; want %s and %s", h.Cc, h.Bcc, want.Cc, want.Bcc)
	}
	if h.MessageID != want.MessageID {
		e.t.Errorf("MessageID = %q, want %q without angle brackets", h.MessageID, want.MessageID)
	}
	if got := mailstore.MessageIDs(h.InReplyTo); !slices.Equal(got, []string{want.InReplyTo}) {
		e.t.Errorf("In-Reply-To = %q", h.InReplyTo)
	}
	if got := mailstore.MessageIDs(h.References); !slices.Equal(got, want.References) {
		e.t.Errorf("References = %q, want %q", h.References, want.References)
	}
	if len(h.Attachments) != len(want.Attachments) {
		e.t.Fatalf("attachments = %+v", h.Attachments)
	}
	for i, a := range want.Attachments {
		wantA := mailstore.Attachment{Part: AttachmentPart(i), Filename: a.Filename, ContentType: a.ContentType, Size: int64(base64.StdEncoding.EncodedLen(len(a.Data)))}
		if h.Attachments[i] != wantA {
			e.t.Errorf("attachment %d = %+v, want %+v", i, h.Attachments[i], wantA)
		}
	}
	if p := hs[0]; len(p.Cc) != 0 || len(p.Bcc) != 0 || len(p.Attachments) != 0 {
		e.t.Errorf("plain header = %+v, want no Cc, Bcc or attachments", p)
	}
	stale := src
	stale.UIDValidity++
	_, err = hf.FetchHeaders(e.ctx, stale, []uint32{r.UID})
	wantErr(e.t, "FetchHeaders with another UIDVALIDITY", err, mailstore.ErrStaleRef)
	if none, err := hf.FetchHeaders(e.ctx, src, nil); err != nil || len(none) != 0 {
		e.t.Errorf("FetchHeaders of no UIDs = %v, %v; want none", none, err)
	}
	// No UIDs asks for nothing, so the source is not checked: a missing
	// folder or another UIDVALIDITY gives nil, nil too.
	for _, uids := range [][]uint32{nil, {}} {
		for what, s := range map[string]mailstore.Source{
			"in a missing folder":      {Folder: "Conformance Missing", UIDValidity: src.UIDValidity},
			"with another UIDVALIDITY": stale,
		} {
			if none, err := hf.FetchHeaders(e.ctx, s, uids); err != nil || none != nil {
				e.t.Errorf("FetchHeaders of %#v %s = %v, %v; want nil, nil", uids, what, none, err)
			}
		}
	}
	e.unread("CfHead")
	e.unread("CfHeadPlain")
}

func searchFolders(e *env) {
	sc, ok := e.s.(mailstore.Scoper)
	if !ok {
		e.t.Fatalf("%T does not implement mailstore.Scoper", e.s)
	}
	for _, in := range []string{"", "inbox", "trash", pick(e.gmail, "spam", "junk"), "anywhere"} {
		got, err := sc.SearchFolders(e.ctx, in)
		if err != nil {
			e.t.Errorf("SearchFolders(%q): %v", in, err)
			continue
		}
		res, err := e.s.Search(e.ctx, mailstore.SearchRequest{In: in, CountOnly: true})
		if err != nil {
			e.t.Errorf("Search(in:%q): %v", in, err)
			continue
		}
		var searched []string
		for _, r := range res {
			searched = append(searched, r.Source.Folder)
		}
		if !slices.Equal(got, searched) {
			e.t.Errorf("SearchFolders(%q) = %q, but Search ran in %q", in, got, searched)
		}
	}
	def, err := sc.SearchFolders(e.ctx, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if !slices.Contains(def, e.home()) || slices.Contains(def, e.trash()) || slices.Contains(def, e.spam()) {
		e.t.Errorf("SearchFolders(\"\") = %q, want %s and neither %s nor %s", def, e.home(), e.trash(), e.spam())
	}
	if got, err := sc.SearchFolders(e.ctx, "trash"); err != nil || !slices.Equal(got, []string{e.trash()}) {
		e.t.Errorf("SearchFolders(trash) = %q, %v; want %s", got, err, e.trash())
	}
	if !e.gmail {
		_, err := sc.SearchFolders(e.ctx, "Conformance Missing")
		wantErr(e.t, "SearchFolders of a missing folder", err, mailstore.ErrNotFound)
	}
}

// sameSummary reports whether two summaries of one message agree, the
// times compared as instants, since two reads may parse them into
// different locations.
func sameSummary(a, b mailstore.Summary) bool {
	return a.Ref == b.Ref && a.Date.Equal(b.Date) && a.InternalDate.Equal(b.InternalDate) &&
		slices.Equal(a.From, b.From) && slices.Equal(a.To, b.To) && a.Subject == b.Subject &&
		slices.Equal(a.Flags, b.Flags) && a.Size == b.Size && a.HasAttachment == b.HasAttachment &&
		a.ThreadID == b.ThreadID && slices.Equal(a.Labels, b.Labels)
}

func sortRefs(refs []ref.Ref) {
	slices.SortFunc(refs, func(a, b ref.Ref) int {
		return cmp.Or(cmp.Compare(a.Folder, b.Folder), cmp.Compare(a.UID, b.UID))
	})
}
