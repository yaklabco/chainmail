package setup

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// The files bootstrap writes on NixOS.
const (
	ServiceNixFile = "chainmail.nix"
	CredFile       = "chainmail-vault-key.cred"
	// CredName is the systemd credential name the module loads.
	CredName = "vault-key"
)

// nixHeader is the comment and function head every Nix file bootstrap
// writes starts with.
const nixHeader = "# Made by chainmail bootstrap. Import it from your NixOS configuration.\n" +
	"{ ... }:\n" +
	"{\n"

// AccountNixFile is the file name of account name's module.
func AccountNixFile(name string) string { return "chainmail-" + name + ".nix" }

// RenderServiceNix is chainmail.nix: the service enabled, clientUser in
// clientUsers, and the credential next to it.
func RenderServiceNix(clientUser string) []byte {
	var b strings.Builder
	b.WriteString(nixHeader)
	b.WriteString("  services.chainmail = {\n")
	b.WriteString("    enable = true;\n")
	b.WriteString("    clientUsers = [ " + nixString(clientUser) + " ];\n")
	b.WriteString("    vaultKeyCredential = ./" + CredFile + ";\n")
	b.WriteString("  };\n}\n")
	return []byte(b.String())
}

// RenderAccountNix is chainmail-<name>.nix: a module setting
// services.chainmail.settings.accounts.<name> from a, the same fields
// accountTOML writes, and oauth_clients.<client>.client_id when c is
// not nil.
func RenderAccountNix(a AccountSpec, c *OAuthClientSpec) []byte {
	var b strings.Builder
	b.WriteString(nixHeader)
	b.WriteString("  services.chainmail.settings = {\n")
	b.WriteString("    accounts." + nixAttr(a.Name) + " = {\n")
	b.WriteString("      provider = " + nixString(a.Provider) + ";\n")
	b.WriteString("      address = " + nixString(a.Address) + ";\n")
	b.WriteString("      auth = " + nixString(string(a.Auth)) + ";\n")
	if a.OAuthClient != "" {
		b.WriteString("      oauth_client = " + nixString(a.OAuthClient) + ";\n")
	}
	for _, o := range []struct {
		key string
		ep  *config.EndpointOverride
	}{{"imap", a.IMAP}, {"smtp", a.SMTP}} {
		if o.ep != nil {
			b.WriteString("      " + o.key + " = " + endpointNix(*o.ep) + ";\n")
		}
	}
	b.WriteString("    };\n")
	if c != nil {
		b.WriteString("    oauth_clients." + nixAttr(c.Name) + ".client_id = " + nixString(c.ClientID) + ";\n")
	}
	b.WriteString("  };\n}\n")
	return []byte(b.String())
}

// endpointNix is an endpoint override as a one-line attribute set, with
// the fields and the tls default accountTOML uses.
func endpointNix(ep config.EndpointOverride) string {
	security := ep.Security
	if security == "" {
		security = provider.SecurityTLS.String()
	}
	s := fmt.Sprintf("{ host = %s; port = %d; security = %s;", nixString(ep.Host), ep.Port, nixString(security))
	if ep.ServerName != "" {
		s += " tls_server_name = " + nixString(ep.ServerName) + ";"
	}
	return s + " }"
}

// nixString quotes s as a Nix double-quoted string, escaping \, " and
// ${, and writing \n, \r and \t for those control characters. It works
// on bytes, so s comes back byte for byte when Nix reads the result. The
// one exception is NUL, which a Nix string cannot hold.
func nixString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '$':
			if i+1 < len(s) && s[i+1] == '{' {
				b.WriteString(`\$`)
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// nixIdentRE matches a Nix identifier.
var nixIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_'-]*$`)

// nixKeywords are the identifiers Nix reserves. As attribute names they
// are a syntax error unquoted, except or, which is quoted all the same.
var nixKeywords = map[string]bool{
	"assert": true, "else": true, "if": true, "in": true, "inherit": true,
	"let": true, "or": true, "rec": true, "then": true, "with": true,
}

// nixAttr is name as a Nix attribute name: bare when it matches
// [A-Za-z_][A-Za-z0-9_'-]* and is not a keyword, else nixString(name).
func nixAttr(name string) string {
	if nixIdentRE.MatchString(name) && !nixKeywords[name] {
		return name
	}
	return nixString(name)
}
