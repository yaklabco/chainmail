package setup

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

func TestRenderServiceNix(t *testing.T) {
	t.Parallel()
	const want = `# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail = {
    enable = true;
    clientUsers = [ "alice" ];
    vaultKeyCredential = ./chainmail-vault-key.cred;
  };
}
`
	if got := string(RenderServiceNix("alice")); got != want {
		t.Errorf("RenderServiceNix(alice):\n--- got\n%s\n--- want\n%s", got, want)
	}

	// ValidName rejects such a name, but the renderer must still quote it.
	got := string(RenderServiceNix(`al"ice`))
	if !strings.Contains(got, `    clientUsers = [ "al\"ice" ];`+"\n") {
		t.Errorf("RenderServiceNix(al\"ice) does not quote the user:\n%s", got)
	}
}

func TestAccountNixFile(t *testing.T) {
	t.Parallel()
	if got := AccountNixFile("work-mail"); got != "chainmail-work-mail.nix" {
		t.Errorf("AccountNixFile(work-mail) = %q", got)
	}
}

// nixCase is one golden pair: testdata/nixos/<name>.nix from
// RenderAccountNix and testdata/nixos/<name>.toml from NewConfig.
type nixCase struct {
	name   string
	spec   AccountSpec
	client *OAuthClientSpec
}

// googleClient is the OAuth client the workspace cases use.
var googleClient = &OAuthClientSpec{Name: "google", ClientID: "1234.apps.googleusercontent.com"}

// nixCases lists the golden pairs. nix/bootstrap-render.nix lists the same
// names, so a case added here is added there too.
var nixCases = []nixCase{
	{
		name: "gmail-password",
		spec: AccountSpec{Name: "alice", Provider: "gmail", Address: "alice@gmail.com", Auth: provider.AuthPassword},
	},
	{
		name: "icloud-password",
		spec: AccountSpec{Name: "alice", Provider: "icloud", Address: "alice@icloud.com", Auth: provider.AuthPassword},
	},
	{
		name: "hostinger-password",
		spec: AccountSpec{Name: "alice", Provider: "hostinger", Address: "alice@example.org", Auth: provider.AuthPassword},
	},
	{
		name: "workspace-oauth",
		spec: AccountSpec{
			Name: "alice", Provider: "google-workspace", Address: "alice@example.com",
			Auth: provider.AuthOAuth2, OAuthClient: "google",
		},
		client: googleClient,
	},
	{
		// The client is already configured, so the module leaves it out.
		name: "workspace-existing-client",
		spec: AccountSpec{
			Name: "alice", Provider: "google-workspace", Address: "alice@example.com",
			Auth: provider.AuthOAuth2, OAuthClient: "google",
		},
	},
	{
		// The address holds the escapes the client ID does not: $${,
		// a $ before an escaped quote, a tab, a raw control byte and a
		// trailing $.
		name: "quoting",
		spec: AccountSpec{
			Name: "work-mail", Provider: "google-workspace", Address: "w$${o}\t$\"\x01@example.com$",
			Auth: provider.AuthOAuth2, OAuthClient: "google",
		},
		client: &OAuthClientSpec{Name: "google", ClientID: `a"b\c${d}`},
	},
	{
		// A Nix keyword is a valid account name and must be quoted.
		name: "keyword-name",
		spec: AccountSpec{Name: "in", Provider: "hostinger", Address: "in@example.org", Auth: provider.AuthPassword},
	},
	{
		// Endpoint overrides exist only under the e2e build tag. SMTP
		// leaves security empty, which renders as tls.
		name: "endpoint-override",
		spec: AccountSpec{
			Name: "alice", Provider: "hostinger", Address: "alice@example.org", Auth: provider.AuthPassword,
			IMAP: &config.EndpointOverride{Host: "127.0.0.1", Port: 1143, Security: "starttls"},
			SMTP: &config.EndpointOverride{Host: "127.0.0.1", Port: 1465, ServerName: "mail.example.org"},
		},
	},
}

func TestRenderAccountNixGolden(t *testing.T) {
	t.Parallel()
	for _, tt := range nixCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			golden(t, "nixos/"+tt.name+".nix", RenderAccountNix(tt.spec, tt.client))
		})
	}
}

// TestNixTOMLGoldenPairs keeps the TOML half of each pair equal to what
// bootstrap writes to config.toml on other Linux hosts.
func TestNixTOMLGoldenPairs(t *testing.T) {
	t.Parallel()
	for _, tt := range nixCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			golden(t, "nixos/"+tt.name+".toml", NewConfig(LayoutFor(Linux), tt.spec, tt.client))
		})
	}
}

// TestRenderAccountNixWorkspace pins the contract's example, which -update
// cannot rewrite the way it rewrites a golden file.
func TestRenderAccountNixWorkspace(t *testing.T) {
	t.Parallel()
	const want = `# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.alice = {
      provider = "google-workspace";
      address = "alice@example.com";
      auth = "oauth2";
      oauth_client = "google";
    };
    oauth_clients.google.client_id = "1234.apps.googleusercontent.com";
  };
}
`
	spec := AccountSpec{
		Name: "alice", Provider: "google-workspace", Address: "alice@example.com",
		Auth: provider.AuthOAuth2, OAuthClient: "google",
	}
	if got := string(RenderAccountNix(spec, googleClient)); got != want {
		t.Errorf("RenderAccountNix:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestNixString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{in: "", want: `""`},
		{in: `a"b`, want: `"a\"b"`},
		{in: `a\b`, want: `"a\\b"`},
		{in: "${x}", want: `"\${x}"`},
		{in: "$x", want: `"$x"`},
		{in: "$", want: `"$"`},
		{in: "$${x}", want: `"$\${x}"`},
		{in: "\n", want: `"\n"`},
		{in: "\r\t", want: `"\r\t"`},
	}
	for _, tt := range tests {
		t.Run(strconv.Quote(tt.in), func(t *testing.T) {
			t.Parallel()
			if got := nixString(tt.in); got != tt.want {
				t.Errorf("nixString(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestNixAttr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{in: "alice", want: "alice"},
		{in: "work-mail", want: "work-mail"},
		{in: "_x", want: "_x"},
		{in: "a'b", want: "a'b"},
		{in: "1abc", want: `"1abc"`},
		{in: "a.b", want: `"a.b"`},
		{in: "a b", want: `"a b"`},
		{in: "", want: `""`},
		{in: "in", want: `"in"`},
		{in: "let", want: `"let"`},
		{in: "or", want: `"or"`},
		{in: "inherit", want: `"inherit"`},
		{in: "inner", want: "inner"},
	}
	for _, tt := range tests {
		t.Run(strconv.Quote(tt.in), func(t *testing.T) {
			t.Parallel()
			if got := nixAttr(tt.in); got != tt.want {
				t.Errorf("nixAttr(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func FuzzNixString(f *testing.F) {
	for _, s := range []string{"", `a"b`, `a\b`, "${x}", "$${x}", "$", "\\${", "\n\r\t", "héllo", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsRune(s, 0) {
			t.Skip("Nix strings cannot hold NUL")
		}
		q := nixString(s)
		if len(q) < 2 || q[0] != '"' || q[len(q)-1] != '"' {
			t.Fatalf("nixString(%q) = %q is not double-quoted", s, q)
		}
		got, err := unquoteNix(q[1 : len(q)-1])
		if err != nil {
			t.Fatalf("nixString(%q) = %q: %v", s, q, err)
		}
		if got != s {
			t.Fatalf("nixString(%q) = %q decodes to %q", s, q, got)
		}
	})
}

// unquoteNix decodes the inside of a Nix double-quoted string as written
// by nixString. It fails on an unescaped quote or ${, which would end the
// string or start an interpolation, and on escapes nixString never writes.
func unquoteNix(body string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '"':
			return "", errors.New("unescaped quote")
		case '$':
			if i+1 < len(body) && body[i+1] == '{' {
				return "", errors.New("unescaped ${")
			}
			b.WriteByte(c)
		case '\\':
			i++
			if i == len(body) {
				return "", errors.New("trailing backslash")
			}
			switch body[i] {
			case '\\', '"', '$':
				b.WriteByte(body[i])
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				return "", errors.New("unknown escape \\" + string(body[i]))
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}
