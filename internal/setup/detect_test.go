package setup

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// fakeResolver answers MX lookups from a table and counts the calls.
type fakeResolver struct {
	mx    []*net.MX
	err   error
	calls int
}

func (f *fakeResolver) LookupMX(_ context.Context, _ string) ([]*net.MX, error) {
	f.calls++
	return f.mx, f.err
}

func asRefusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("got error %v (%T), want *Refusal", err, err)
	}
	return r
}

func TestParseAddress(t *testing.T) {
	local, domain, err := ParseAddress("Omer.Mail@GMAIL.com")
	if err != nil {
		t.Fatalf("ParseAddress: %v", err)
	}
	if local != "Omer.Mail" || domain != "gmail.com" {
		t.Fatalf("got %q, %q; want Omer.Mail, gmail.com", local, domain)
	}

	refused := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no at", "noat"},
		{"no domain", "a@"},
		{"no local", "@b"},
		{"two ats", "a@b@c"},
		{"display name", "Name <a@b.com>"},
		{"angle brackets", "<a@b.com>"},
		{"space in domain", "a@b .com"},
		{"comment", "a@b.com (work)"},
		{"leading space", " a@b.com"},
		{"too long", strings.Repeat("a", 64) + "@" + strings.Repeat("b", 186) + ".com"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseAddress(tc.in)
			r := asRefusal(t, err)
			if r.Msg == "" {
				t.Fatal("refusal has no message")
			}
		})
	}
}

func TestParseAddressLengthLimit(t *testing.T) {
	// 254 bytes is the longest address allowed.
	ok := strings.Repeat("a", 59) + "@" + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62) + ".com"
	if len(ok) != 254 {
		t.Fatalf("test address is %d bytes, want 254", len(ok))
	}
	if _, _, err := ParseAddress(ok); err != nil {
		t.Fatalf("254-byte address refused: %v", err)
	}
	if _, _, err := ParseAddress("a" + ok); err == nil {
		t.Fatal("255-byte address accepted")
	}
}

func TestAccountName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"omermail@gmail.com", "omermail"},
		{"Omer.Mail+tag@x.com", "omer-mail-tag"},
		{"__x@y.com", "x"},
		{"-.9lives@y.com", "9lives"},
		{strings.Repeat("a", 70) + "@y.com", strings.Repeat("a", 63)},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := AccountName(tc.in)
			if err != nil {
				t.Fatalf("AccountName: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if !config.ValidName(got) {
				t.Fatalf("%q fails config.ValidName", got)
			}
		})
	}

	t.Run("nothing valid", func(t *testing.T) {
		_, err := AccountName("___@y.com")
		r := asRefusal(t, err)
		if !strings.Contains(r.Hint, "--name") {
			t.Fatalf("hint %q does not name --name", r.Hint)
		}
	})

	t.Run("bad address", func(t *testing.T) {
		_, err := AccountName("noat")
		_ = asRefusal(t, err)
	})
}

func TestDetect(t *testing.T) {
	ctx := context.Background()

	t.Run("domain list", func(t *testing.T) {
		cases := []struct {
			address string
			want    string
		}{
			{"a@gmail.com", "gmail"},
			{"a@googlemail.com", "gmail"},
			{"a@GMail.com", "gmail"},
			{"a@icloud.com", "icloud"},
			{"a@me.com", "icloud"},
			{"a@mac.com", "icloud"},
		}
		for _, tc := range cases {
			t.Run(tc.address, func(t *testing.T) {
				r := &fakeResolver{err: errors.New("must not be called")}
				got, err := Detect(ctx, r, tc.address, "")
				if err != nil {
					t.Fatalf("Detect: %v", err)
				}
				want := Detection{Provider: tc.want, Auth: provider.AuthPassword}
				if got != want {
					t.Fatalf("got %+v, want %+v", got, want)
				}
				if r.calls != 0 {
					t.Fatalf("resolver called %d times, want 0", r.calls)
				}
			})
		}
	})

	t.Run("mx", func(t *testing.T) {
		cases := []struct {
			name string
			mx   []*net.MX
			want Detection
		}{
			{
				name: "google",
				mx:   []*net.MX{{Host: "aspmx.l.google.com.", Pref: 1}},
				want: Detection{Provider: "google-workspace", Auth: provider.AuthOAuth2, ViaMX: true},
			},
			{
				name: "googlemail",
				mx:   []*net.MX{{Host: "alt1.aspmx.l.googlemail.com.", Pref: 5}},
				want: Detection{Provider: "google-workspace", Auth: provider.AuthOAuth2, ViaMX: true},
			},
			{
				name: "google without trailing dot, mixed case",
				mx:   []*net.MX{{Host: "ASPMX.L.Google.COM", Pref: 1}},
				want: Detection{Provider: "google-workspace", Auth: provider.AuthOAuth2, ViaMX: true},
			},
			{
				name: "hostinger",
				mx:   []*net.MX{{Host: "mx1.hostinger.com.", Pref: 5}, {Host: "mx2.hostinger.com.", Pref: 10}},
				want: Detection{Provider: "hostinger", Auth: provider.AuthPassword, ViaMX: true},
			},
			{
				name: "lowest preference wins",
				mx:   []*net.MX{{Host: "mx1.hostinger.com.", Pref: 20}, nil, {Host: "aspmx.l.google.com.", Pref: 1}},
				want: Detection{Provider: "google-workspace", Auth: provider.AuthOAuth2, ViaMX: true},
			},
			{
				name: "unknown host before a known one",
				mx:   []*net.MX{{Host: "mx.example.net.", Pref: 1}, {Host: "mx1.hostinger.com.", Pref: 5}},
				want: Detection{Provider: "hostinger", Auth: provider.AuthPassword, ViaMX: true},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := &fakeResolver{mx: tc.mx}
				got, err := Detect(ctx, r, "me@example.org", "")
				if err != nil {
					t.Fatalf("Detect: %v", err)
				}
				if got != tc.want {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
				if r.calls != 1 {
					t.Fatalf("resolver called %d times, want 1", r.calls)
				}
			})
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		cases := []struct {
			name string
			mx   []*net.MX
		}{
			{"label boundary", []*net.MX{{Host: "evilgoogle.com.", Pref: 1}}},
			{"suffix in the middle", []*net.MX{{Host: "google.com.evil.net.", Pref: 1}}},
			{"hostinger label boundary", []*net.MX{{Host: "nothostinger.com.", Pref: 1}}},
			{"no records", nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := Detect(ctx, &fakeResolver{mx: tc.mx}, "me@example.org", "")
				r := asRefusal(t, err)
				want := "unknown provider for example.org; pass --provider <preset>"
				if r.Msg != want {
					t.Fatalf("message %q, want %q", r.Msg, want)
				}
				for _, p := range BootstrapProviders() {
					if !strings.Contains(r.Hint, p) {
						t.Fatalf("hint %q does not list %s", r.Hint, p)
					}
				}
			})
		}
	})

	t.Run("domain not found is a DNS failure", func(t *testing.T) {
		nf := &net.DNSError{Err: "no such host", Name: "example.org", IsNotFound: true}
		_, err := Detect(ctx, &fakeResolver{err: nf}, "me@example.org", "")
		r := asRefusal(t, err)
		if !strings.Contains(r.Msg, "DNS lookup") || !strings.Contains(r.Msg, "failed") || !strings.Contains(r.Msg, "no such host") {
			t.Fatalf("message %q, want a DNS failure naming the resolver error", r.Msg)
		}
	})

	t.Run("dns error", func(t *testing.T) {
		_, err := Detect(ctx, &fakeResolver{err: errors.New("i/o timeout")}, "me@example.org", "")
		r := asRefusal(t, err)
		if !strings.Contains(r.Msg, "DNS lookup") || !strings.Contains(r.Msg, "failed") {
			t.Fatalf("message %q does not say the DNS lookup failed", r.Msg)
		}
		if !strings.Contains(r.Msg, "example.org") {
			t.Fatalf("message %q does not name the domain", r.Msg)
		}
		if !strings.Contains(r.Hint, "--provider") {
			t.Fatalf("hint %q does not name --provider", r.Hint)
		}
	})

	t.Run("bad address", func(t *testing.T) {
		r := &fakeResolver{}
		_, err := Detect(ctx, r, "Name <a@b.com>", "")
		_ = asRefusal(t, err)
		if r.calls != 0 {
			t.Fatalf("resolver called %d times, want 0", r.calls)
		}
	})

	t.Run("override", func(t *testing.T) {
		cases := []struct {
			override string
			want     Detection
		}{
			{"hostinger", Detection{Provider: "hostinger", Auth: provider.AuthPassword}},
			{"gmail", Detection{Provider: "gmail", Auth: provider.AuthPassword}},
			{"icloud", Detection{Provider: "icloud", Auth: provider.AuthPassword}},
			{"google-workspace", Detection{Provider: "google-workspace", Auth: provider.AuthOAuth2}},
		}
		for _, tc := range cases {
			t.Run(tc.override, func(t *testing.T) {
				r := &fakeResolver{err: errors.New("must not be called")}
				got, err := Detect(ctx, r, "me@example.org", tc.override)
				if err != nil {
					t.Fatalf("Detect: %v", err)
				}
				if got != tc.want {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
				if r.calls != 0 {
					t.Fatalf("resolver called %d times, want 0", r.calls)
				}
			})
		}
	})

	t.Run("override custom", func(t *testing.T) {
		r := &fakeResolver{}
		_, err := Detect(ctx, r, "me@example.org", "custom")
		ref := asRefusal(t, err)
		const page = "https://chainmail.sh/docs/getting-started/accounts"
		if !strings.Contains(ref.Msg+ref.Hint, page) {
			t.Fatalf("refusal %q, hint %q does not point at %s", ref.Msg, ref.Hint, page)
		}
		if r.calls != 0 {
			t.Fatalf("resolver called %d times, want 0", r.calls)
		}
	})

	t.Run("override unknown", func(t *testing.T) {
		r := &fakeResolver{}
		_, err := Detect(ctx, r, "me@example.org", "nope")
		ref := asRefusal(t, err)
		if !strings.Contains(ref.Msg, "nope") {
			t.Fatalf("message %q does not name the provider", ref.Msg)
		}
		if !strings.Contains(ref.Hint, strings.Join(BootstrapProviders(), ", ")) {
			t.Fatalf("hint %q does not list %v", ref.Hint, BootstrapProviders())
		}
		if r.calls != 0 {
			t.Fatalf("resolver called %d times, want 0", r.calls)
		}
	})
}

func TestAppPasswordURL(t *testing.T) {
	cases := map[string]string{
		"gmail":            "https://myaccount.google.com/apppasswords",
		"icloud":           "https://account.apple.com",
		"hostinger":        "",
		"google-workspace": "",
		"custom":           "",
		"":                 "",
	}
	for p, want := range cases {
		if got := AppPasswordURL(p); got != want {
			t.Errorf("AppPasswordURL(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestBootstrapProviders(t *testing.T) {
	got := BootstrapProviders()
	want := []string{"gmail", "google-workspace", "hostinger", "icloud"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, name := range got {
		p, ok := provider.Lookup(name)
		if !ok {
			t.Fatalf("%s is not a provider preset", name)
		}
		d, err := Detect(context.Background(), &fakeResolver{}, "me@example.org", name)
		if err != nil {
			t.Fatalf("Detect override %s: %v", name, err)
		}
		if !p.SupportsAuth(d.Auth) {
			t.Fatalf("preset %s does not support auth %s", name, d.Auth)
		}
	}
	got[0] = "changed"
	if BootstrapProviders()[0] != "gmail" {
		t.Fatal("BootstrapProviders returned a shared slice")
	}
}
