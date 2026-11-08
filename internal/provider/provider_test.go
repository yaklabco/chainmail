package provider

import (
	"slices"
	"testing"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		imap   Endpoint
		smtp   Endpoint
		auth   []AuthMethod
		oauth2 bool
		gmail  bool
	}{
		{
			name:   "google-workspace",
			imap:   Endpoint{Host: "imap.gmail.com", Port: 993, Security: SecurityTLS},
			smtp:   Endpoint{Host: "smtp.gmail.com", Port: 587, Security: SecurityStartTLS},
			auth:   []AuthMethod{AuthOAuth2},
			oauth2: true,
			gmail:  true,
		},
		{
			name:   "gmail",
			imap:   Endpoint{Host: "imap.gmail.com", Port: 993, Security: SecurityTLS},
			smtp:   Endpoint{Host: "smtp.gmail.com", Port: 587, Security: SecurityStartTLS},
			auth:   []AuthMethod{AuthOAuth2, AuthPassword},
			oauth2: true,
			gmail:  true,
		},
		{
			name: "hostinger",
			imap: Endpoint{Host: "imap.hostinger.com", Port: 993, Security: SecurityTLS},
			smtp: Endpoint{Host: "smtp.hostinger.com", Port: 465, Security: SecurityTLS},
			auth: []AuthMethod{AuthPassword},
		},
		{
			name: "icloud",
			imap: Endpoint{Host: "imap.mail.me.com", Port: 993, Security: SecurityTLS},
			smtp: Endpoint{Host: "smtp.mail.me.com", Port: 587, Security: SecurityStartTLS},
			auth: []AuthMethod{AuthPassword},
		},
		{
			name: "custom",
			auth: []AuthMethod{AuthPassword},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, ok := Lookup(tt.name)
			if !ok {
				t.Fatalf("Lookup(%q) not found", tt.name)
			}
			if p.Name != tt.name {
				t.Errorf("Name = %q, want %q", p.Name, tt.name)
			}
			if p.IMAP != tt.imap {
				t.Errorf("IMAP = %+v, want %+v", p.IMAP, tt.imap)
			}
			if p.SMTP != tt.smtp {
				t.Errorf("SMTP = %+v, want %+v", p.SMTP, tt.smtp)
			}
			if !slices.Equal(p.AuthMethods, tt.auth) {
				t.Errorf("AuthMethods = %v, want %v", p.AuthMethods, tt.auth)
			}
			if got := p.OAuth2 != nil; got != tt.oauth2 {
				t.Errorf("OAuth2 set = %v, want %v", got, tt.oauth2)
			}
			if p.Gmail != tt.gmail {
				t.Errorf("Gmail = %v, want %v", p.Gmail, tt.gmail)
			}
			if p.MaxConnections <= 0 {
				t.Errorf("MaxConnections = %d, want > 0", p.MaxConnections)
			}
		})
	}
}

func TestLookupUnknown(t *testing.T) {
	t.Parallel()
	if _, ok := Lookup("aol"); ok {
		t.Error("Lookup(aol) found a preset")
	}
}

func TestGooglePresetsUseOAuth2(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"google-workspace", "gmail"} {
		p, _ := Lookup(name)
		if p.OAuth2 == nil {
			t.Fatalf("%s: OAuth2 is nil", name)
		}
		if !slices.Equal(p.OAuth2.Scopes, []string{"https://mail.google.com/"}) {
			t.Errorf("%s: scopes = %v", name, p.OAuth2.Scopes)
		}
		if p.OAuth2.AuthURL != "https://accounts.google.com/o/oauth2/auth" {
			t.Errorf("%s: auth URL = %q", name, p.OAuth2.AuthURL)
		}
		if p.OAuth2.TokenURL != "https://oauth2.googleapis.com/token" {
			t.Errorf("%s: token URL = %q", name, p.OAuth2.TokenURL)
		}
	}
}

func TestLookupReturnsCopies(t *testing.T) {
	t.Parallel()
	p, _ := Lookup("gmail")
	p.AuthMethods[0] = "mutated"
	p.OAuth2.Scopes[0] = "mutated"
	again, _ := Lookup("gmail")
	if again.AuthMethods[0] != AuthOAuth2 || again.OAuth2.Scopes[0] != "https://mail.google.com/" {
		t.Error("mutating a looked-up preset changed the registry")
	}
}

func TestParseSecurity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    Security
		wantErr bool
	}{
		{in: "tls", want: SecurityTLS},
		{in: "starttls", want: SecurityStartTLS},
		{in: "ssl", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseSecurity(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSecurity(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSecurity(%q) = %v, want %v", tt.in, got, tt.want)
		}
		if !tt.wantErr && got.String() != tt.in {
			t.Errorf("String() = %q, want %q", got.String(), tt.in)
		}
	}
}

func TestEndpointAddr(t *testing.T) {
	t.Parallel()
	if got := (Endpoint{Host: "::1", Port: 993}).Addr(); got != "[::1]:993" {
		t.Errorf("Addr() = %q", got)
	}
}

func TestNames(t *testing.T) {
	t.Parallel()
	want := []string{"custom", "gmail", "google-workspace", "hostinger", "icloud"}
	if got := Names(); !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestSupportsAuth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		preset string
		method AuthMethod
		want   bool
	}{
		{preset: "google-workspace", method: AuthOAuth2, want: true},
		{preset: "google-workspace", method: AuthPassword, want: false},
		{preset: "gmail", method: AuthOAuth2, want: true},
		{preset: "gmail", method: AuthPassword, want: true},
		{preset: "hostinger", method: AuthPassword, want: true},
		{preset: "hostinger", method: AuthOAuth2, want: false},
		{preset: "icloud", method: AuthOAuth2, want: false},
		{preset: "custom", method: AuthPassword, want: true},
		{preset: "custom", method: AuthOAuth2, want: false},
	}
	for _, tt := range tests {
		p, ok := Lookup(tt.preset)
		if !ok {
			t.Fatalf("Lookup(%q) not found", tt.preset)
		}
		if got := p.SupportsAuth(tt.method); got != tt.want {
			t.Errorf("%s.SupportsAuth(%s) = %v, want %v", tt.preset, tt.method, got, tt.want)
		}
	}
}

func TestPoolAndSizeDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pool    int
		maxSize int64
	}{
		{name: "google-workspace", pool: 6, maxSize: 25 << 20},
		{name: "gmail", pool: 6, maxSize: 25 << 20},
		{name: "icloud", pool: 3, maxSize: 35 << 20},
		{name: "hostinger", pool: 3, maxSize: 35 << 20},
		{name: "custom", pool: 3, maxSize: 25 << 20},
	}
	for _, tt := range tests {
		p, ok := Lookup(tt.name)
		if !ok {
			t.Fatalf("Lookup(%q) not found", tt.name)
		}
		if p.DefaultPool != tt.pool {
			t.Errorf("%s: DefaultPool = %d, want %d", tt.name, p.DefaultPool, tt.pool)
		}
		if p.MaxMessageSize != tt.maxSize {
			t.Errorf("%s: MaxMessageSize = %d, want %d", tt.name, p.MaxMessageSize, tt.maxSize)
		}
	}
	if got := len(Names()); got != len(tests) {
		t.Errorf("Names() has %d presets, the test covers %d", got, len(tests))
	}
}

func TestDefaultPoolFitsConnectionLimit(t *testing.T) {
	t.Parallel()
	for _, name := range Names() {
		p, _ := Lookup(name)
		if p.DefaultPool < 1 || p.DefaultPool > p.MaxConnections {
			t.Errorf("%s: DefaultPool = %d, want 1 to MaxConnections %d", name, p.DefaultPool, p.MaxConnections)
		}
	}
}

func TestEndpointTLSServerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ep   Endpoint
		want string
	}{
		{name: "host", ep: Endpoint{Host: "imap.example.com", Port: 993}, want: "imap.example.com"},
		{
			name: "server name",
			ep:   Endpoint{Host: "nlb.internal.example", Port: 993, ServerName: "mail.example.com"},
			want: "mail.example.com",
		},
	}
	for _, tt := range tests {
		if got := tt.ep.TLSServerName(); got != tt.want {
			t.Errorf("%s: TLSServerName() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestGoogleCalendarScopes(t *testing.T) {
	t.Parallel()
	want := []string{
		"https://www.googleapis.com/auth/calendar.events",
		"https://www.googleapis.com/auth/calendar.calendarlist.readonly",
		"https://www.googleapis.com/auth/calendar.freebusy",
	}
	if !slices.Equal(GoogleCalendarScopes, want) {
		t.Errorf("GoogleCalendarScopes = %v, want %v", GoogleCalendarScopes, want)
	}
	for _, name := range []string{"google-workspace", "gmail"} {
		p, _ := Lookup(name)
		for _, s := range GoogleCalendarScopes {
			if slices.Contains(p.OAuth2.Scopes, s) {
				t.Errorf("%s preset scopes hold %s; calendar scopes are per account", name, s)
			}
		}
	}
}

// The custom preset takes oauth2 only through an OAuth client with an
// issuer, whose endpoints come from discovery. Every other preset either
// has its own endpoints or takes no oauth2 at all.
func TestAcceptsAuth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		preset string
		method AuthMethod
		issuer bool
		want   bool
	}{
		{preset: "custom", method: AuthPassword, issuer: false, want: true},
		{preset: "custom", method: AuthOAuth2, issuer: false, want: false},
		{preset: "custom", method: AuthOAuth2, issuer: true, want: true},
		{preset: "hostinger", method: AuthOAuth2, issuer: true, want: false},
		{preset: "icloud", method: AuthOAuth2, issuer: true, want: false},
		{preset: "google-workspace", method: AuthOAuth2, issuer: false, want: true},
		{preset: "google-workspace", method: AuthOAuth2, issuer: true, want: false},
		{preset: "gmail", method: AuthPassword, issuer: false, want: true},
	}
	for _, tt := range tests {
		p, ok := Lookup(tt.preset)
		if !ok {
			t.Fatalf("Lookup(%q) not found", tt.preset)
		}
		if got := p.AcceptsAuth(tt.method, tt.issuer); got != tt.want {
			t.Errorf("%s.AcceptsAuth(%s, issuer %v) = %v, want %v", tt.preset, tt.method, tt.issuer, got, tt.want)
		}
	}
}
