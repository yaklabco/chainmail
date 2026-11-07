package cli

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// ruleFile is a rule file, the TOML document rule create -f, rule edit -f
// and rule dry-run -f read and rule show --toml writes: RuleDefinition's
// fields by their proto names, then one [[actions]] table per action.
// Priority and Enabled are pointers, so a file that leaves them out gets
// the daemon's defaults.
type ruleFile struct {
	Name        string           `toml:"name"`
	Description string           `toml:"description"`
	Accounts    []string         `toml:"accounts"`
	Query       string           `toml:"query"`
	OnNew       bool             `toml:"on_new"`
	Schedule    string           `toml:"schedule"`
	Priority    *int32           `toml:"priority"`
	Stop        bool             `toml:"stop"`
	MaxPerRun   int32            `toml:"max_per_run"`
	Enabled     *bool            `toml:"enabled"`
	Actions     []ruleFileAction `toml:"actions"`
}

// ruleFileAction is one [[actions]] table: the kind and the arguments it
// takes, by RuleAction's field names.
type ruleFileAction struct {
	Kind   string   `toml:"kind"`
	Label  string   `toml:"label,omitempty"`
	Folder string   `toml:"folder,omitempty"`
	ItemID string   `toml:"item_id,omitempty"`
	Anchor bool     `toml:"anchor,omitempty"`
	To     []string `toml:"to,omitempty"`
	Note   string   `toml:"note,omitempty"`
}

// ruleFileOf is r as a rule file with every key set, so editing a rule
// with the file it printed replaces every field.
func ruleFileOf(r *chainmailv1.Rule) *ruleFile {
	f := &ruleFile{
		Name: r.GetName(), Description: r.GetDescription().GetUntrusted(), Accounts: r.GetAccounts(),
		Query: r.GetQuery().GetUntrusted(), OnNew: r.GetOnNew(), Schedule: r.GetSchedule(),
		Priority: proto.Int32(r.GetPriority()), Stop: r.GetStop(), MaxPerRun: r.GetMaxPerRun(), Enabled: proto.Bool(r.GetEnabled()),
	}
	for _, a := range r.GetActions() {
		f.Actions = append(f.Actions, ruleFileAction{
			Kind: a.GetKind(), Label: a.GetLabel(), Folder: a.GetFolder(), ItemID: a.GetItemId(),
			Anchor: a.GetAnchor(), To: a.GetTo(), Note: a.GetNote(),
		})
	}
	return f
}

// writeRuleFile writes r as a rule file. TOML escapes control characters,
// so a description or query copied from mail cannot drive the terminal.
func writeRuleFile(w io.Writer, r *chainmailv1.Rule) error {
	b, err := toml.Marshal(ruleFileOf(r))
	if err != nil {
		return fmt.Errorf("encoding the rule file: %w", err)
	}
	return writeString(w, string(b))
}

// ruleMaskOrder is the RuleDefinition fields in the proto's order: the
// keys a rule file may hold, and the order rule edit lists its UpdateRule
// mask paths in, so a request never depends on the order of the flags.
var ruleMaskOrder = []string{"name", "description", "accounts", "query", "on_new", "schedule", "priority", "stop", "actions", "max_per_run", "enabled"}

// definition is the RuleDefinition the file gives.
func (f *ruleFile) definition() *chainmailv1.RuleDefinition {
	d := &chainmailv1.RuleDefinition{
		Name: f.Name, Description: f.Description, Accounts: f.Accounts, Query: f.Query, OnNew: f.OnNew,
		Schedule: f.Schedule, Priority: f.Priority, Stop: f.Stop, MaxPerRun: f.MaxPerRun, Enabled: f.Enabled,
	}
	for _, a := range f.Actions {
		d.Actions = append(d.Actions, &chainmailv1.RuleAction{
			Kind: a.Kind, Label: a.Label, Folder: a.Folder, ItemId: a.ItemID, Anchor: a.Anchor, To: a.To, Note: a.Note,
		})
	}
	return d
}

// readRuleFile reads the rule file at path for cmd. It returns the
// definition the file gives and the RuleDefinition fields it sets, in
// ruleMaskOrder. A file that does not parse, or holds a key a rule file
// does not have, is a usage error pointing at chainmail rule's help, which
// describes the file.
func readRuleFile(cmd *cobra.Command, path string) (*chainmailv1.RuleDefinition, []string, error) {
	b, err := readFile(path, "--file")
	if err != nil {
		return nil, nil, err
	}
	notRule := func(cause error) error {
		return usageError{err: fmt.Errorf("%s is not a rule file: %w", path, cause), hint: helpHint(cmd.Parent())}
	}
	var keys map[string]any
	if err := toml.Unmarshal(b, &keys); err != nil {
		return nil, nil, notRule(err)
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if !slices.Contains(ruleMaskOrder, k) {
			return nil, nil, notRule(fmt.Errorf("unknown key %q", k))
		}
	}
	var f ruleFile
	if err := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&f); err != nil {
		return nil, nil, notRule(err)
	}
	var paths []string
	for _, p := range ruleMaskOrder {
		if _, ok := keys[p]; ok {
			paths = append(paths, p)
		}
	}
	return f.definition(), paths, nil
}
