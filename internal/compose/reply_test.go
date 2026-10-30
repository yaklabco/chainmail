package compose

import (
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	replyAlice = mail.Address{Name: "Alice Example", Address: "alice@example.com"}
	replyBob   = mail.Address{Name: "Bob", Address: "bob@example.com"}
	replyCarol = mail.Address{Address: "carol@example.com"}
	replyMe    = mail.Address{Name: "Me", Address: "me@example.com"}
)

// replyZone is a fixed zone, so attribution dates do not depend on the machine.
var replyZone = time.FixedZone("test", -5*60*60)

func replyOriginal() Original {
	return Original{
		MessageID:  "orig@example.com",
		References: []string{"root@example.com", "mid@example.com"},
		From:       []mail.Address{replyAlice},
		To:         []mail.Address{replyMe, replyBob},
		Cc:         []mail.Address{replyCarol},
		Subject:    "Lunch",
		Date:       time.Date(2026, 10, 30, 17, 4, 0, 0, time.UTC),
		Text:       "Are you free?\n",
		HTML:       "<p>Are you free?</p>",
	}
}

func replyAddrs(as []mail.Address) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Address
	}
	return out
}

func TestReplyRecipients(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		orig   func(*Original)
		self   []string
		all    bool
		wantTo []string
		wantCc []string
	}{
		{
			name:   "plain sender",
			self:   []string{"me@example.com"},
			wantTo: []string{"alice@example.com"},
		},
		{
			name:   "reply-to wins",
			orig:   func(o *Original) { o.ReplyTo = []mail.Address{{Address: "list@example.com"}} },
			self:   []string{"me@example.com"},
			wantTo: []string{"list@example.com"},
		},
		{
			name:   "reply all adds to and cc without self",
			self:   []string{"me@example.com"},
			all:    true,
			wantTo: []string{"alice@example.com"},
			wantCc: []string{"bob@example.com", "carol@example.com"},
		},
		{
			name: "reply all drops self in any case and dedups",
			orig: func(o *Original) {
				o.To = []mail.Address{{Address: "ME@Example.COM"}, replyBob, {Address: "Bob@example.com"}}
				o.Cc = []mail.Address{{Address: "ALICE@example.com"}, replyCarol, {Address: "Me.Alias@example.com"}, replyCarol}
			},
			self:   []string{"me@example.com", "me.alias@EXAMPLE.com"},
			all:    true,
			wantTo: []string{"alice@example.com"},
			wantCc: []string{"bob@example.com", "carol@example.com"},
		},
		{
			name: "reply all to own message goes to its recipients",
			orig: func(o *Original) {
				o.From = []mail.Address{replyMe}
				o.To = []mail.Address{replyBob}
			},
			self:   []string{"me@example.com"},
			all:    true,
			wantTo: []string{"bob@example.com"},
			wantCc: []string{"carol@example.com"},
		},
		{
			name: "reply to own message goes to its To",
			orig: func(o *Original) {
				o.From = []mail.Address{replyMe}
				o.To = []mail.Address{replyBob}
			},
			self:   []string{"me@example.com"},
			wantTo: []string{"bob@example.com"},
		},
		{
			name: "reply all to own message with only cc keeps cc",
			orig: func(o *Original) {
				o.From = []mail.Address{replyMe}
				o.To = []mail.Address{replyMe}
			},
			self:   []string{"me@example.com"},
			all:    true,
			wantCc: []string{"carol@example.com"},
		},
		{
			name: "duplicate senders collapse to the first",
			orig: func(o *Original) {
				o.ReplyTo = []mail.Address{replyAlice, {Address: "Alice@Example.com"}}
			},
			self:   []string{"me@example.com"},
			wantTo: []string{"alice@example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := replyOriginal()
			if tt.orig != nil {
				tt.orig(&o)
			}
			got, err := Reply(o, tt.self, tt.all, replyZone)
			if err != nil {
				t.Fatalf("Reply: %v", err)
			}
			if to := replyAddrs(got.To); !slices.Equal(to, tt.wantTo) {
				t.Errorf("To = %q, want %q", to, tt.wantTo)
			}
			if cc := replyAddrs(got.Cc); !slices.Equal(cc, tt.wantCc) {
				t.Errorf("Cc = %q, want %q", cc, tt.wantCc)
			}
		})
	}
}

func TestReplyKeepsNames(t *testing.T) {
	t.Parallel()
	got, err := Reply(replyOriginal(), []string{"me@example.com"}, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.To) != 1 || got.To[0] != replyAlice {
		t.Errorf("To = %v, want [%v]", got.To, replyAlice)
	}
}

func TestReplyNoRecipients(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		orig func(*Original)
		all  bool
	}{
		{
			name: "reply all where only self remains",
			orig: func(o *Original) {
				o.From = []mail.Address{replyMe}
				o.To = []mail.Address{{Address: "Me@Example.com"}}
				o.Cc = nil
			},
			all: true,
		},
		{
			name: "no sender",
			orig: func(o *Original) { o.From = nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := replyOriginal()
			tt.orig(&o)
			_, err := Reply(o, []string{"me@example.com"}, tt.all, replyZone)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Reply error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestReplySubject(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"Lunch", "Re: Lunch"},
		{"Re: Lunch", "Re: Lunch"},
		{"RE: Lunch", "RE: Lunch"},
		{"re: Lunch", "re: Lunch"},
		{"  re:Lunch", "re:Lunch"},
		{"Fwd: Lunch", "Re: Fwd: Lunch"},
		{"Regarding lunch", "Re: Regarding lunch"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			o := replyOriginal()
			o.Subject = tt.in
			got, err := Reply(o, nil, false, replyZone)
			if err != nil {
				t.Fatal(err)
			}
			if got.Subject != tt.want {
				t.Errorf("Subject = %q, want %q", got.Subject, tt.want)
			}
		})
	}
}

func TestForwardSubject(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"Lunch", "Fwd: Lunch"},
		{"Fwd: Lunch", "Fwd: Lunch"},
		{"FWD: Lunch", "FWD: Lunch"},
		{"fw: Lunch", "fw: Lunch"},
		{"FW:Lunch", "FW:Lunch"},
		{"Re: Lunch", "Fwd: Re: Lunch"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			o := replyOriginal()
			o.Subject = tt.in
			if got := Forward(o, replyZone).Subject; got != tt.want {
				t.Errorf("Subject = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReplyThreading(t *testing.T) {
	t.Parallel()
	got, err := Reply(replyOriginal(), nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if got.InReplyTo != "orig@example.com" {
		t.Errorf("InReplyTo = %q, want orig@example.com", got.InReplyTo)
	}
	want := []string{"root@example.com", "mid@example.com", "orig@example.com"}
	if !slices.Equal(got.References, want) {
		t.Errorf("References = %q, want %q", got.References, want)
	}
}

func TestReplyReferencesTrimmed(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.References = nil
	for i := range 20 {
		o.References = append(o.References, fmt.Sprintf("r%02d@example.com", i))
	}
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	// 20 references plus the Message-ID is 21 IDs, so the oldest goes.
	if len(got.References) != 20 {
		t.Fatalf("len(References) = %d, want 20", len(got.References))
	}
	if got.References[0] != "r01@example.com" {
		t.Errorf("References[0] = %q, want r01@example.com", got.References[0])
	}
	if got.References[19] != "orig@example.com" {
		t.Errorf("References[19] = %q, want orig@example.com", got.References[19])
	}
	if o.References[0] != "r00@example.com" {
		t.Errorf("Reply changed the original's References: %q", o.References)
	}
}

func TestReplyNoMessageID(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.MessageID = ""
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if got.InReplyTo != "" {
		t.Errorf("InReplyTo = %q, want empty", got.InReplyTo)
	}
	if !slices.Equal(got.References, o.References) {
		t.Errorf("References = %q, want %q", got.References, o.References)
	}
}

func TestReplyQuote(t *testing.T) {
	t.Parallel()
	got, err := Reply(replyOriginal(), nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	q := got.Quote
	if q == nil {
		t.Fatal("Quote is nil")
	}
	// 17:04 UTC is 12:04 in a zone five hours behind.
	if want := "On Fri, 30 Oct 2026 at 12:04, Alice Example <alice@example.com> wrote:"; q.Header != want {
		t.Errorf("Header = %q, want %q", q.Header, want)
	}
	if q.Text != "Are you free?\n" {
		t.Errorf("Text = %q", q.Text)
	}
	if q.HTML != "<p>Are you free?</p>" {
		t.Errorf("HTML = %q", q.HTML)
	}
	if q.Forward {
		t.Error("Forward is set on a reply quote")
	}
}

func TestReplyQuoteSenderWithoutName(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.From = []mail.Address{replyCarol}
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if want := "On Fri, 30 Oct 2026 at 12:04, carol@example.com wrote:"; got.Quote.Header != want {
		t.Errorf("Header = %q, want %q", got.Quote.Header, want)
	}
}

func TestReplyQuoteCleansHeaderValues(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.From = []mail.Address{{Name: "Eve\r\nBcc: x@evil.example\u202e", Address: "eve@example.com"}}
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	h := got.Quote.Header
	if strings.ContainsAny(h, "\r\n\u202e") {
		t.Errorf("Header %q holds a line break or invisible character", h)
	}
	if !strings.Contains(h, "Eve Bcc: x@evil.example <eve@example.com> wrote:") {
		t.Errorf("Header = %q", h)
	}
}

func TestReplyQuoteSanitizesHTML(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.HTML = `<p>hi</p><script>alert(1)</script>`
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Quote.HTML, "<script") || strings.Contains(got.Quote.HTML, "alert(1)") {
		t.Errorf("Quote.HTML = %q, want the script dropped", got.Quote.HTML)
	}
	if !strings.Contains(got.Quote.HTML, "<p>hi</p>") {
		t.Errorf("Quote.HTML = %q, lacks the paragraph", got.Quote.HTML)
	}
}

func TestReplyQuoteWithoutHTML(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.HTML = ""
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if got.Quote.HTML != "" {
		t.Errorf("Quote.HTML = %q, want empty so the text is used", got.Quote.HTML)
	}
}

func TestForward(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.HTML = `<p>hi</p><script>alert(1)</script>`
	o.Attachments = []File{
		{Filename: "menu.pdf", ContentType: "application/pdf", Data: []byte("%PDF")},
		{Filename: "map.png", ContentType: "image/png", Data: []byte("PNG")},
	}
	got := Forward(o, replyZone)
	q := got.Quote
	if q == nil {
		t.Fatal("Quote is nil")
	}
	want := "---------- Forwarded message ---------\n" +
		"From: Alice Example <alice@example.com>\n" +
		"Date: Fri, 30 Oct 2026 at 12:04\n" +
		"Subject: Lunch\n" +
		"To: Me <me@example.com>, Bob <bob@example.com>"
	if q.Header != want {
		t.Errorf("Header =\n%s\nwant\n%s", q.Header, want)
	}
	if !q.Forward {
		t.Error("Forward is not set on a forward quote")
	}
	if q.Text != o.Text {
		t.Errorf("Text = %q, want %q", q.Text, o.Text)
	}
	if strings.Contains(q.HTML, "<script") || !strings.Contains(q.HTML, "<p>hi</p>") {
		t.Errorf("Quote.HTML = %q, want sanitized", q.HTML)
	}
	if len(got.Attachments) != 2 || got.Attachments[0].Filename != "menu.pdf" || got.Attachments[1].Filename != "map.png" {
		t.Errorf("Attachments = %v, want the original's", got.Attachments)
	}
}

func TestForwardCleansHeaderValues(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.Subject = "Lunch\r\nBcc: x@evil.example"
	h := Forward(o, replyZone).Quote.Header
	if !strings.Contains(h, "\nSubject: Lunch Bcc: x@evil.example\n") {
		t.Errorf("Header = %q, want the subject on one line", h)
	}
}

func TestQuoteWithoutDate(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.Date = time.Time{}
	got, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Alice Example <alice@example.com> wrote:"; got.Quote.Header != want {
		t.Errorf("reply Header = %q, want %q", got.Quote.Header, want)
	}
	if h := Forward(o, replyZone).Quote.Header; !strings.Contains(h, "\nDate: \n") {
		t.Errorf("forward Header = %q, want an empty Date", h)
	}
}

// A note to self leaves no one to reply to, but the caller can still name
// recipients: Reply reports ErrNoRecipients and fills in everything else.
func TestReplyNoRecipientsStillFillsTheParts(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.From = []mail.Address{replyMe}
	o.To = []mail.Address{replyMe}
	o.Cc = nil
	p, err := Reply(o, []string{"me@example.com"}, false, replyZone)
	if !errors.Is(err, ErrNoRecipients) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrNoRecipients wrapping ErrInvalid", err)
	}
	if len(p.To) != 0 || len(p.Cc) != 0 {
		t.Errorf("recipients = %v, %v, want none", p.To, p.Cc)
	}
	if p.Subject != "Re: Lunch" || p.InReplyTo != "orig@example.com" || p.Quote == nil {
		t.Errorf("parts = %+v, want the subject, In-Reply-To and quote filled in", p)
	}
}

// A truncated original's quote ends with TruncatedMarker in its text and,
// after sanitizing, in its HTML, so HTML that ends inside a comment cannot
// swallow the marker. An original that is whole gets no marker.
func TestQuoteMarksATruncatedOriginal(t *testing.T) {
	t.Parallel()
	o := replyOriginal()
	o.Text = "first line\nsecond line\n"
	o.HTML = "<p>visible</p><!-- cut inside a comment"
	o.Truncated = true
	reply, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	fwd := Forward(o, replyZone)
	for name, q := range map[string]*Quote{"reply": reply.Quote, "forward": fwd.Quote} {
		if want := "first line\nsecond line\n" + TruncatedMarker + "\n"; q.Text != want {
			t.Errorf("%s: Text = %q, want %q", name, q.Text, want)
		}
		if want := "<p>visible</p>\n<p>" + TruncatedMarker + "</p>\n"; q.HTML != want {
			t.Errorf("%s: HTML = %q, want %q", name, q.HTML, want)
		}
	}

	o.Truncated = false
	whole, err := Reply(o, nil, false, replyZone)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(whole.Quote.Text, TruncatedMarker) || strings.Contains(whole.Quote.HTML, TruncatedMarker) {
		t.Errorf("a whole original's quote = %+v, want no marker", whole.Quote)
	}
}
