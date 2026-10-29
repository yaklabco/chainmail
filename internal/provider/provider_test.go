package provider

import (
	"slices"
	"testing"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		imap  Endpoint
		smtp  Endpoint
		auth  []AuthMethod
		gmail bool
	}{
		{
			name:  "google-workspace",
			imap:  Endpoint{Host: "imap.gmail.com", Port: 993, Security: SecurityTLS},
			smtp:  Endpoint{Host: "smtp.gmail.com", Port: 587, Security: SecurityStartTLS},
			auth:  []AuthMethod{AuthOAuth2},
			gmail: true,
		},
		{
			name:  "gmail",
			imap:  Endpoint{Host: "imap.gmail.com", Port: 993, Security: SecurityTLS},
			smtp:  Endpoint{Host: "smtp.gmail.com", Port: 587, Security: SecurityStartTLS},
			auth:  []AuthMethod{AuthOAuth2, AuthPassword},
			gmail: true,
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
