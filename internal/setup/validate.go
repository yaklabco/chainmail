package setup

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// validateHint is the hint on every plan Validate refuses.
const validateHint = "run chainmail bootstrap again; bootstrap apply only takes plans bootstrap made"

// userNameRE matches the user names a plan may name: portable POSIX names.
var userNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// Validate is the apply-side check: platform, version, every Entry known
// to l, user and group names equal to l's, account fields valid, overrides
// only when e2eEnabled. It returns a *Refusal.
//
// Everything is checked against l and this binary's own rendering, never
// against the plan: the service text must equal RenderService(l), and
// the config text must parse and hold exactly the plan's account. The
// apply step also rebuilds the plan from the installation it inspects
// under the lock and compares the two with Same, which ties the config
// text to what is on disk.
func Validate(l Layout, p Plan) error {
	if p.Version != planVersion {
		return refuse(validateHint, "plan version %d is not %d", p.Version, planVersion)
	}
	if p.Platform != l.Platform {
		return refuse(validateHint, "plan is for %q, this host is %q", p.Platform, l.Platform)
	}
	if !userNameRE.MatchString(p.InvokingUser) || p.InvokingUser == "root" || p.InvokingUser == l.DaemonUser {
		return refuse(validateHint, "plan names invoking user %q", p.InvokingUser)
	}
	if err := validateAccount(p); err != nil {
		return err
	}
	if _, err := ParseBackup(string(p.Backup)); err != nil {
		return err
	}

	stores, logins := 0, 0
	for _, a := range p.Actions {
		if err := validateAction(l, p, a); err != nil {
			return err
		}
		switch a.Kind {
		case ActStoreSecret:
			stores++
		case ActAuthLogin:
			logins++
		}
	}
	var wantSteps []string
	if p.Account.Auth == provider.AuthOAuth2 && p.NeedSecret && logins == 0 {
		wantSteps = []string{"chainmail auth login " + p.Account.Name}
	}
	if !slices.Equal(p.NextSteps, wantSteps) {
		return refuse(validateHint, "plan has next steps %q", p.NextSteps)
	}
	if p.NeedSecret != (stores == 1) || stores > 1 {
		return refuse(validateHint, "plan has %d store-secret steps with NeedSecret %v", stores, p.NeedSecret)
	}
	return nil
}

// validateAccount checks the account, its OAuth client, its secret name
// and its overrides.
func validateAccount(p Plan) error {
	a := p.Account
	if !config.ValidName(a.Name) {
		return refuse(validateHint, "plan names account %q", a.Name)
	}
	if _, _, err := ParseAddress(a.Address); err != nil {
		return err
	}
	if err := checkProvider(a.Provider, a.Auth); err != nil {
		return err
	}
	if err := checkOverrides(a.IMAP, a.SMTP); err != nil {
		return err
	}
	switch a.Auth {
	case provider.AuthPassword:
		if a.OAuthClient != "" || p.OAuthClient != nil {
			return refuse(validateHint, "plan gives password account %q an OAuth client", a.Name)
		}
	case provider.AuthOAuth2:
		c := p.OAuthClient
		if !config.ValidName(a.OAuthClient) || c == nil || c.Name != a.OAuthClient || c.ClientID == "" {
			return refuse(validateHint, "plan gives OAuth account %q no valid client", a.Name)
		}
	}
	if p.SecretName != secretName(a) {
		return refuse(validateHint, "plan stores secret %q for account %q", p.SecretName, a.Name)
	}
	return nil
}

// validateAction checks one action against l and the plan's account.
func validateAction(l Layout, p Plan, a Action) error {
	bad := func(why string) error {
		return refuse(validateHint, "plan step %s %s%s: %s", a.Kind, a.Entry, a.Name, why)
	}
	if a.Note == "" || strings.ContainsFunc(a.Note, unicode.IsControl) {
		return bad("has an empty note or one with control characters")
	}
	if a.Data != nil && a.Kind != ActWriteConfig && a.Kind != ActWriteService {
		return bad("carries file contents")
	}
	switch a.Kind {
	case ActCreateGroup:
		return wantTarget(a, "", l.ClientGroup, bad)
	case ActCreateUser:
		return wantTarget(a, "", l.DaemonUser, bad)
	case ActAddMember:
		if a.Name != p.InvokingUser && a.Name != l.DaemonUser {
			return bad("adds a user other than the invoking user or the daemon user")
		}
		return wantTarget(a, "", a.Name, bad)
	case ActEnsureDir:
		e, ok := l.Entry(a.Entry)
		if !ok || e.Kind != KindDir || a.Name != "" {
			return bad("is not a directory of the layout")
		}
	case ActInstallBin:
		return wantTarget(a, "binary", "", bad)
	case ActCreateKey:
		return wantTarget(a, "vault-key", "", bad)
	case ActWriteConfig:
		if err := wantTarget(a, "config", "", bad); err != nil {
			return err
		}
		return validateConfig(p, a.Data, bad)
	case ActWriteService:
		if err := wantTarget(a, "service", "", bad); err != nil {
			return err
		}
		if !bytes.Equal(a.Data, RenderService(l)) {
			return bad("service definition differs from the one this binary writes")
		}
	case ActActivate:
		return wantTarget(a, "", "", bad)
	case ActStoreSecret, ActTestAccount:
		return wantTarget(a, "", p.Account.Name, bad)
	case ActAuthLogin:
		if p.Account.Auth != provider.AuthOAuth2 {
			return bad("logs in to an account that does not use OAuth")
		}
		return wantTarget(a, "", p.Account.Name, bad)
	default:
		return bad("unknown step")
	}
	return nil
}

// wantTarget refuses an action whose Entry or Name is not the one given.
func wantTarget(a Action, entry, name string, bad func(string) error) error {
	if a.Entry != entry || a.Name != name {
		return bad("names the wrong target")
	}
	return nil
}

// validateConfig checks that data parses and holds the plan's account,
// and its OAuth client, exactly.
func validateConfig(p Plan, data []byte, bad func(string) error) error {
	cfg, err := config.Parse(data)
	if err != nil {
		return bad("config does not parse")
	}
	want := p.Account
	got, ok := cfg.Accounts[want.Name]
	if !ok {
		return bad("config does not hold the account")
	}
	if got.Provider != want.Provider || got.Address != want.Address ||
		provider.AuthMethod(got.Auth) != want.Auth || got.OAuthClient != want.OAuthClient ||
		!sameEndpoint(got.IMAP, withSecurity(want.IMAP)) || !sameEndpoint(got.SMTP, withSecurity(want.SMTP)) {
		return bad("config account differs from the plan's")
	}
	if c := p.OAuthClient; c != nil && cfg.OAuthClients[c.Name].ClientID != c.ClientID {
		return bad("config OAuth client differs from the plan's")
	}
	return nil
}

// withSecurity returns ep with config.Parse's default security applied.
func withSecurity(ep *config.EndpointOverride) *config.EndpointOverride {
	if ep == nil || ep.Security != "" {
		return ep
	}
	c := *ep
	c.Security = provider.SecurityTLS.String()
	return &c
}
