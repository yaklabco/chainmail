package setup

import (
	"cmp"
	"context"
	"net"
	"net/mail"
	"slices"
	"strings"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// Resolver looks up MX records. *net.Resolver satisfies it.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Detection is the provider bootstrap picked for an address.
type Detection struct {
	Provider string              // a key of provider.Lookup
	Auth     provider.AuthMethod // password or oauth2
	ViaMX    bool                // true when the MX lookup decided it
}

// maxAddressLen is the longest address SMTP allows (RFC 5321 path limit
// less the angle brackets).
const maxAddressLen = 254

// maxNameLen is the longest account name config.ValidName accepts.
const maxNameLen = 63

// accountsPage documents accounts bootstrap does not set up, such as custom.
const accountsPage = "https://chainmail.sh/docs/getting-started/accounts"

// bootstrapAuth maps each preset bootstrap supports to the auth method it
// sets up. It is the source of BootstrapProviders.
var bootstrapAuth = map[string]provider.AuthMethod{
	"gmail":            provider.AuthPassword,
	"google-workspace": provider.AuthOAuth2,
	"hostinger":        provider.AuthPassword,
	"icloud":           provider.AuthPassword,
}

// knownDomains maps consumer mail domains to their preset, decided without
// a DNS lookup.
var knownDomains = map[string]string{
	"gmail.com":      "gmail",
	"googlemail.com": "gmail",
	"icloud.com":     "icloud",
	"me.com":         "icloud",
	"mac.com":        "icloud",
}

// mxSuffixes maps MX host domains to their preset. A host matches a suffix
// when it equals it or ends in "." plus it.
var mxSuffixes = []struct {
	suffix   string
	provider string
}{
	{"google.com", "google-workspace"},
	{"googlemail.com", "google-workspace"},
	{"hostinger.com", "hostinger"},
}

// appPasswordURLs are the pages that make app passwords, by preset.
var appPasswordURLs = map[string]string{
	"gmail":  "https://myaccount.google.com/apppasswords",
	"icloud": "https://account.apple.com",
}

// ParseAddress splits a bare address ("local@domain") and lowercases the
// domain. Anything else is a *Refusal.
func ParseAddress(s string) (local, domain string, err error) {
	const hint = "pass a bare address such as name@example.com"
	if len(s) > maxAddressLen {
		return "", "", refuse(hint, "address is %d bytes; the limit is %d", len(s), maxAddressLen)
	}
	addr, perr := mail.ParseAddress(s)
	if perr != nil || addr.Name != "" || addr.Address != s {
		return "", "", refuse(hint, "%q is not a bare email address", s)
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return "", "", refuse(hint, "%q is not a bare email address", s)
	}
	return s[:at], strings.ToLower(s[at+1:]), nil
}

// AccountName derives the default account name from the local part, as
// the spec's "Account name" section says: lowercased, every character
// outside [a-z0-9-] replaced by "-", leading characters outside [a-z0-9]
// dropped, and cut to 63 characters. A result that fails config.ValidName
// is a *Refusal asking for --name.
func AccountName(address string) (string, error) {
	local, _, err := ParseAddress(address)
	if err != nil {
		return "", err
	}
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(local))
	name = strings.TrimLeft(name, "-")
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	if !config.ValidName(name) {
		return "", refuse("pass --name <account> with lowercase letters, digits and dashes",
			"cannot derive an account name from %q", local)
	}
	return name, nil
}

// Detect picks the provider. override is --provider, "" when not given.
// The domain list decides first, then the domain's MX records through r.
func Detect(ctx context.Context, r Resolver, address, override string) (Detection, error) {
	_, domain, err := ParseAddress(address)
	if err != nil {
		return Detection{}, err
	}
	if override != "" {
		return detectOverride(override)
	}
	if p, ok := knownDomains[domain]; ok {
		return Detection{Provider: p, Auth: bootstrapAuth[p]}, nil
	}

	records, err := r.LookupMX(ctx, domain)
	if err != nil {
		return Detection{}, refuse(providerHint(),
			"DNS lookup of the MX records for %s failed: %v", domain, err)
	}
	if p, ok := matchMX(records); ok {
		return Detection{Provider: p, Auth: bootstrapAuth[p], ViaMX: true}, nil
	}
	return Detection{}, refuse(providerHint(),
		"unknown provider for %s; pass --provider <preset>", domain)
}

// detectOverride turns --provider into a Detection.
func detectOverride(name string) (Detection, error) {
	if name == "custom" {
		return Detection{}, refuse("set up custom accounts by hand: "+accountsPage,
			"bootstrap does not set up custom provider accounts")
	}
	auth, ok := bootstrapAuth[name]
	if !ok {
		return Detection{}, refuse(providerHint(), "bootstrap does not support provider %q", name)
	}
	return Detection{Provider: name, Auth: auth}, nil
}

// matchMX returns the preset of the most preferred MX host that matches
// one of mxSuffixes on a label boundary.
func matchMX(records []*net.MX) (string, bool) {
	sorted := slices.DeleteFunc(slices.Clone(records), func(mx *net.MX) bool { return mx == nil })
	slices.SortStableFunc(sorted, func(a, b *net.MX) int { return cmp.Compare(a.Pref, b.Pref) })
	for _, mx := range sorted {
		host := strings.ToLower(strings.TrimSuffix(mx.Host, "."))
		for _, s := range mxSuffixes {
			if host == s.suffix || strings.HasSuffix(host, "."+s.suffix) {
				return s.provider, true
			}
		}
	}
	return "", false
}

// providerHint lists the presets --provider accepts.
func providerHint() string {
	return "pass --provider <preset>, one of: " + strings.Join(BootstrapProviders(), ", ")
}

// AppPasswordURL is the page that makes an app password for provider, or "".
func AppPasswordURL(provider string) string {
	return appPasswordURLs[provider]
}

// BootstrapProviders lists the presets bootstrap supports, sorted.
func BootstrapProviders() []string {
	names := make([]string, 0, len(bootstrapAuth))
	for name := range bootstrapAuth {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
