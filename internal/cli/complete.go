package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/provider"
)

// completionTimeout bounds each daemon lookup a completion makes, so a
// daemon that is slow or stuck never hangs the shell.
const completionTimeout = 1500 * time.Millisecond

// completionDescWidth is the most runes a candidate's description shows.
const completionDescWidth = 60

// completer answers tab completion, from the daemon where the answer lives
// there. A lookup that fails offers nothing and says why only in
// BASH_COMP_DEBUG_FILE.
type completer struct {
	env Env
	g   *globals
}

// call runs fn against the daemon under completionTimeout, with the socket
// cmd's flags, the environment and cli.toml name. It returns false when the
// daemon could not answer. cobra gives the command it completes Execute's
// context, so cmd.Context() carries cancellation.
//
//nolint:contextcheck // ctx is cmd.Context(), which cobra (completions.go) sets to Execute's context before any completion function runs; contextcheck cannot see that.
func (cp *completer) call(cmd *cobra.Command, fn func(context.Context, *client.Client) error) bool {
	if err := cp.g.resolve(cp.env, cmd); err != nil {
		cobra.CompDebugln("chainmail: "+err.Error(), false)
		return false
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	c := cp.g.client(cp.env)
	defer c.Close()
	if err := fn(ctx, c); err != nil {
		cobra.CompDebugln("chainmail: "+err.Error(), false)
		return false
	}
	return true
}

// completionText makes text from mail or a server fit for a description:
// one line, no control or formatting characters, at most
// completionDescWidth runes. Tab and newline would otherwise split cobra's
// value, description and record fields.
func completionText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return clip(termText(s), completionDescWidth)
}

// safeValue says s can be offered as a candidate as it is. A value that
// needed cleaning would not name the same thing once cleaned, so it is
// left out instead. That includes leading or trailing whitespace, which
// cobra trims from each candidate line.
func safeValue(s string) bool {
	return s != "" && strings.TrimFunc(s, unicode.IsSpace) == s && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}

// noComplete marks an argument or flag that takes free text.
func noComplete(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// files lets the shell complete file names.
func files(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveDefault
}

// dirs lets the shell complete directory names.
func dirs(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}

// fixed completes from a fixed list, filtered by what has been typed.
func fixed(choices ...cobra.Completion) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return withPrefix(choices, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// withPrefix keeps the candidates whose value starts with prefix.
func withPrefix(cands []cobra.Completion, prefix string) []cobra.Completion {
	out := []cobra.Completion{}
	for _, c := range cands {
		value, _, _ := strings.Cut(c, "\t")
		if strings.HasPrefix(value, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// commaList completes a comma-separated list of values, such as
// --kind new,flags, without repeating a value already in the list.
func commaList(values ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		done, last := "", toComplete
		if i := strings.LastIndex(toComplete, ","); i >= 0 {
			done, last = toComplete[:i+1], toComplete[i+1:]
		}
		chosen := strings.Split(strings.TrimSuffix(done, ","), ",")
		out := []cobra.Completion{}
		for _, v := range values {
			if strings.HasPrefix(v, last) && !slices.Contains(chosen, v) {
				out = append(out, done+v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
}

// inlineFiles completes --inline NAME=FILE: nothing before the =, file
// names after it.
func inlineFiles(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	const directive = cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	name, path, ok := strings.Cut(toComplete, "=")
	if !ok {
		return nil, directive
	}
	dir, base := filepath.Split(path)
	entries, err := os.ReadDir(cmp.Or(dir, "."))
	if err != nil {
		return nil, directive
	}
	out := []cobra.Completion{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) || (strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(base, ".")) {
			continue
		}
		v := name + "=" + dir + e.Name()
		if e.IsDir() {
			v += "/"
		}
		if safeValue(e.Name()) && safeValue(v) {
			out = append(out, v)
		}
	}
	return out, directive
}

// passLimits bounds one walk of the password store.
type passLimits struct {
	// visits is the most files and directories the walk looks at.
	visits int
	// candidates is the most entries it returns.
	candidates int
}

// passCompletionLimits keeps a huge password store from stalling the shell.
var passCompletionLimits = passLimits{visits: 10000, candidates: 500}

// passEntries completes --from-pass with the entries of the password
// store: $PASSWORD_STORE_DIR when set, relative or not, else
// $HOME/.password-store, as checkPassEntry and pass itself decide.
func (cp *completer) passEntries(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	store := cp.env.Getenv("PASSWORD_STORE_DIR")
	if store == "" {
		home := cp.env.Getenv("HOME")
		if home == "" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		store = filepath.Join(home, ".password-store")
	}
	return walkPassStore(cmd.Context(), store, toComplete, passCompletionLimits), cobra.ShellCompDirectiveNoFileComp
}

// walkPassStore returns the sorted entries of the password store at store
// that start with toComplete, within lim. It follows a symlinked store but
// no symlink inside it, and descends only into directories that can hold
// a match. It stops early when ctx is done.
func walkPassStore(ctx context.Context, store, toComplete string, lim passLimits) []string {
	root, err := filepath.EvalSymlinks(store)
	if err != nil {
		cobra.CompDebugln("chainmail: password store: "+err.Error(), false)
		return nil
	}
	var out []string
	visits := 0
	// WalkDir's own error is only ever the SkipAll or a callback error,
	// and the callback returns none: what it found is in out.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if visits++; visits > lim.visits {
			cobra.CompDebugln(fmt.Sprintf("chainmail: password store: stopped after %d files", lim.visits), false)
			return filepath.SkipAll
		}
		if err != nil {
			// WalkDir reports errors only for the store itself or a
			// directory it could not read: skip it, so an unreadable
			// corner of the store offers nothing.
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		// path is under root, which WalkDir walked to reach it, so Rel
		// cannot fail.
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Only a directory on the way to toComplete, or under it,
			// can hold a matching entry.
			canMatch := strings.HasPrefix(toComplete, rel+"/") || strings.HasPrefix(rel, toComplete)
			if strings.HasPrefix(d.Name(), ".") || !canMatch {
				return filepath.SkipDir
			}
			return nil
		}
		entry, isEntry := strings.CutSuffix(rel, ".gpg")
		if !isEntry || !safeValue(entry) || !strings.HasPrefix(entry, toComplete) {
			return nil
		}
		if len(out) == lim.candidates {
			cobra.CompDebugln(fmt.Sprintf("chainmail: password store: stopped at %d entries", lim.candidates), false)
			return filepath.SkipAll
		}
		out = append(out, entry)
		return nil
	})
	slices.Sort(out)
	return out
}

// listAccounts asks the daemon for its accounts.
func (cp *completer) listAccounts(cmd *cobra.Command) ([]*chainmailv1.Account, bool) {
	var accounts []*chainmailv1.Account
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Account.ListAccounts(ctx, connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
		if err != nil {
			return err
		}
		accounts = resp.Msg.GetAccounts()
		return nil
	})
	return accounts, ok
}

// accounts completes account names that pass filter (nil for all), as up
// to maxArgs positional arguments, or a flag value when maxArgs is 0. An
// account already among the arguments is not offered again.
func (cp *completer) accounts(filter func(*chainmailv1.Account) bool, maxArgs int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if maxArgs > 0 && len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		list, ok := cp.listAccounts(cmd)
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []cobra.Completion
		for _, a := range list {
			name := a.GetName()
			if !safeValue(name) || (filter != nil && !filter(a)) || !strings.HasPrefix(name, toComplete) || slices.Contains(args, name) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(name, completionText(a.GetProvider()+" "+a.GetAddress())))
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// oauthAccount keeps accounts that log in with OAuth2.
func oauthAccount(a *chainmailv1.Account) bool {
	return a.GetAuthMethod() == string(provider.AuthOAuth2)
}

// unbounded is maxArgs for a command that takes any number of arguments.
const unbounded = -1

// compSpec is how one command completes: its positional arguments, and
// each of its own flags by name.
type compSpec struct {
	args  cobra.CompletionFunc
	flags map[string]cobra.CompletionFunc
}

// contentFlagSpecs completes addContentFlags' flags.
func contentFlagSpecs() map[string]cobra.CompletionFunc {
	return map[string]cobra.CompletionFunc{
		"body": noComplete, "body-file": files, "html-file": files, "attach": files, "inline": inlineFiles,
	}
}

// recipientFlagSpecs completes --to, --cc and --bcc recipients: there is
// no address book, so they are free text.
func recipientFlagSpecs() map[string]cobra.CompletionFunc {
	return map[string]cobra.CompletionFunc{"to": noComplete, "cc": noComplete, "bcc": noComplete}
}

// mergeFlagCompletions merges maps of flag completions; later maps win.
func mergeFlagCompletions(sets ...map[string]cobra.CompletionFunc) map[string]cobra.CompletionFunc {
	out := map[string]cobra.CompletionFunc{}
	for _, s := range sets {
		maps.Copy(out, s)
	}
	return out
}

// specs is how every command completes, by command path. A test fails for
// a command, positional argument or flag missing from it.
func (cp *completer) specs() map[string]compSpec {
	allAccounts := cp.accounts(nil, 0)
	compose := mergeFlagCompletions(contentFlagSpecs(), recipientFlagSpecs(), map[string]cobra.CompletionFunc{"subject": noComplete})
	inboxRefs := cp.refs("in:inbox", unbounded, sender)
	oneInboxRef := cp.refs("in:inbox", 1, sender)
	oneDraft := cp.refs("in:drafts", 1, recipient)
	refFolders := cp.folders(folderSpec{accounts: argAccounts})
	labels := cp.folders(folderSpec{accounts: cp.googleAccounts, keep: labelFolder})
	existingFolder := cp.folders(folderSpec{accounts: flagAccount, maxArgs: 1})
	answer := mergeFlagCompletions(contentFlagSpecs(), recipientFlagSpecs(), map[string]cobra.CompletionFunc{"idempotency-key": noComplete})
	return map[string]compSpec{
		"chainmail": {args: noComplete, flags: map[string]cobra.CompletionFunc{
			"socket": files,
			"output": fixed(
				cobra.CompletionWithDesc("json", "one line of JSON per result"),
				cobra.CompletionWithDesc("table", "aligned columns for people"),
			),
		}},
		"chainmail accounts":       {args: noComplete},
		"chainmail accounts list":  {args: noComplete},
		"chainmail accounts probe": {args: cp.accounts(nil, unbounded)},
		"chainmail accounts test":  {args: cp.accounts(nil, 1)},
		"chainmail admin":          {args: noComplete},
		"chainmail admin status":   {args: noComplete},
		"chainmail auth":           {args: noComplete},
		"chainmail auth login":     {args: cp.accounts(oauthAccount, 1)},
		"chainmail completion":     {args: fixed("bash", "zsh", "fish", "powershell")},
		"chainmail man":            {args: noComplete},
		"chainmail version":        {args: noComplete},
		"chainmail mcp":            {args: noComplete},
		"chainmail daemon":         {args: noComplete, flags: map[string]cobra.CompletionFunc{"config": files, "state-dir": dirs, "vault-key-file": files}},
		"chainmail draft":          {args: noComplete},
		"chainmail draft create":   {args: noComplete, flags: mergeFlagCompletions(compose, map[string]cobra.CompletionFunc{"account": allAccounts})},
		"chainmail send":           {args: noComplete, flags: mergeFlagCompletions(compose, map[string]cobra.CompletionFunc{"account": allAccounts, "idempotency-key": noComplete})},
		"chainmail send-raw":       {args: fixed(cobra.CompletionWithDesc("-", "read the message from stdin")), flags: map[string]cobra.CompletionFunc{"account": allAccounts, "file": files, "idempotency-key": noComplete}},
		"chainmail folders":        {args: noComplete, flags: map[string]cobra.CompletionFunc{"account": allAccounts}},
		"chainmail secret":         {args: noComplete},
		"chainmail watch groups":   {args: noComplete},
		"chainmail watch status":   {args: noComplete},
		"chainmail watch": {args: noComplete, flags: map[string]cobra.CompletionFunc{
			"account": allAccounts, "kind": commaList(watchKinds...), "since": noComplete,
			"in":    cp.folders(folderSpec{accounts: anyAccount}),
			"group": cp.groups(0),
		}},
		"chainmail watch groups delete": {args: cp.groups(1)},
		"chainmail audit": {args: noComplete, flags: map[string]cobra.CompletionFunc{
			"account": allAccounts, "limit": noComplete, "before-id": noComplete,
			"since": fixed("1h", "24h", "168h"), "action": cp.auditActions,
		}},
		"chainmail search": {args: cp.searchTerms, flags: map[string]cobra.CompletionFunc{
			"account": allAccounts, "cursor": noComplete, "limit": noComplete,
		}},
		"chainmail flags": {args: inboxRefs, flags: map[string]cobra.CompletionFunc{
			"add": fixed(`\Seen`, `\Flagged`, `\Answered`, `\Draft`), "remove": fixed(`\Seen`, `\Flagged`, `\Answered`, `\Draft`),
		}},
		"chainmail secret set":    {args: cp.secretNames, flags: map[string]cobra.CompletionFunc{"from-pass": cp.passEntries}},
		"chainmail get":           {args: inboxRefs},
		"chainmail thread":        {args: oneInboxRef},
		"chainmail attachment":    {args: cp.refThenPart(oneInboxRef), flags: map[string]cobra.CompletionFunc{"out": files}},
		"chainmail archive":       {args: inboxRefs},
		"chainmail trash":         {args: inboxRefs},
		"chainmail expunge":       {args: inboxRefs},
		"chainmail read":          {args: inboxRefs},
		"chainmail unread":        {args: inboxRefs},
		"chainmail flag":          {args: inboxRefs},
		"chainmail unflag":        {args: inboxRefs},
		"chainmail move":          {args: inboxRefs, flags: map[string]cobra.CompletionFunc{"to": refFolders}},
		"chainmail copy":          {args: inboxRefs, flags: map[string]cobra.CompletionFunc{"to": refFolders}},
		"chainmail label":         {args: inboxRefs, flags: map[string]cobra.CompletionFunc{"add": labels, "remove": labels}},
		"chainmail reply":         {args: oneInboxRef, flags: answer},
		"chainmail forward":       {args: oneInboxRef, flags: answer},
		"chainmail draft send":    {args: oneDraft, flags: map[string]cobra.CompletionFunc{"idempotency-key": noComplete}},
		"chainmail draft delete":  {args: oneDraft},
		"chainmail draft update":  {args: oneDraft, flags: compose},
		"chainmail folder":        {args: noComplete, flags: map[string]cobra.CompletionFunc{"account": allAccounts}},
		"chainmail folder create": {args: cp.folders(folderSpec{accounts: flagAccount, maxArgs: 1, noSpace: true})},
		"chainmail folder delete": {args: existingFolder},
		"chainmail folder rename": {args: existingFolder},
	}
}

// applyCompletions registers specs on the tree. A missing args function
// leaves cobra's default, file names, which the coverage test refuses.
func applyCompletions(root *cobra.Command, specs map[string]compSpec) error {
	var errs []error
	walk(root, func(c *cobra.Command) {
		spec, ok := specs[c.CommandPath()]
		if !ok {
			return
		}
		if spec.args != nil {
			c.ValidArgsFunction = spec.args
		}
		for name, fn := range spec.flags {
			if err := c.RegisterFlagCompletionFunc(name, fn); err != nil {
				errs = append(errs, fmt.Errorf("%s --%s: %w", c.CommandPath(), name, err))
			}
		}
	})
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("registering completions: %w", err)
	}
	return nil
}
