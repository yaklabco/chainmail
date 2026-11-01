// Package policy decides what agents may do without James: which recipients
// a send may reach, how many, and whether destructive actions are allowed. It
// is pure, with no I/O.
package policy

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailaddr"
	"github.com/jamesainslie/chainmail/internal/mailaddr/allow"
)

// SendDecision is the outcome of checking a send's recipients.
type SendDecision struct {
	// Allowed is true when nothing is blocked and the count is within the limit.
	Allowed bool
	// Blocked lists the recipients not allowed, in input order and as given.
	Blocked []string
	// TooMany is set when there are more recipients than max_recipients.
	TooMany bool
}

// Policy is the compiled policy config.
type Policy struct {
	// global holds the global send_allow entries. It applies to every account.
	global allowSet
	// own holds every account's address, lowercased. It applies to every
	// account and is matched exactly, never as a *@domain pattern.
	own map[string]struct{}
	// accounts holds each account's own send_allow entries.
	accounts          map[string]allowSet
	maxRecipients     int
	allowExpunge      bool
	allowFolderDelete bool
}

// allowSet is a compiled list of lowercased entries.
type allowSet []string

func (s allowSet) allows(addr string) bool {
	return slices.ContainsFunc(s, func(e string) bool { return allow.Match(e, addr) })
}

// New compiles cfg.Policy, every account's policy and every account
// address. It applies Parse's defaults and entry check through the config
// helpers, so a Config that never went through config.Parse, as in tests,
// meets the same rules, and it returns Parse's error text for an invalid entry.
func New(cfg *config.Config) (*Policy, error) {
	pc := cfg.Policy.WithDefaults()
	p := &Policy{
		own:               make(map[string]struct{}, len(cfg.Accounts)),
		accounts:          make(map[string]allowSet, len(cfg.Accounts)),
		maxRecipients:     pc.MaxRecipients,
		allowExpunge:      pc.AllowExpunge,
		allowFolderDelete: pc.AllowFolderDelete,
	}
	var err error
	if p.global, err = compile("policy.send_allow", pc.SendAllow); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Accounts)) {
		a := cfg.Accounts[name]
		if a.Address != "" {
			p.own[strings.ToLower(a.Address)] = struct{}{}
		}
		set, err := compile("accounts."+name+".policy.send_allow", a.Policy.SendAllow)
		if err != nil {
			return nil, err
		}
		p.accounts[name] = set
	}
	return p, nil
}

func compile(where string, entries []string) (allowSet, error) {
	if problems := config.SendAllowProblems(where, entries); len(problems) > 0 {
		return nil, errors.New(problems[0])
	}
	set := make(allowSet, 0, len(entries))
	for _, e := range entries {
		set = append(set, strings.ToLower(e))
	}
	return set, nil
}

// CheckSend checks the recipients of a send from account. An unknown account
// gets only the global entries and the own addresses. A recipient that is
// not mailaddr.Safe is blocked whatever the allowlist says: the decoded form
// of a quoted local part can carry SMTP path delimiters and parameters, so
// the address SMTP would deliver to is not the one checked here.
func (p *Policy) CheckSend(account string, recipients []string) SendDecision {
	perAccount := p.accounts[account]
	var d SendDecision
	for _, r := range recipients {
		if !mailaddr.Safe(r) || !p.allows(perAccount, strings.ToLower(r)) {
			d.Blocked = append(d.Blocked, r)
		}
	}
	d.TooMany = len(recipients) > p.maxRecipients
	d.Allowed = len(d.Blocked) == 0 && !d.TooMany
	return d
}

// allows reports whether the lowercased addr is an own address or matches
// a global or perAccount entry.
func (p *Policy) allows(perAccount allowSet, addr string) bool {
	_, own := p.own[addr]
	return own || p.global.allows(addr) || perAccount.allows(addr)
}

// AllowExpunge reports whether expunging messages is allowed.
func (p *Policy) AllowExpunge() bool { return p.allowExpunge }

// AllowFolderDelete reports whether deleting folders is allowed.
func (p *Policy) AllowFolderDelete() bool { return p.allowFolderDelete }
