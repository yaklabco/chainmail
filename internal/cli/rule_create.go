package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// ruleFieldFlags are the field flags rule create and rule edit share, in
// the order a conflict with --file names them.
var ruleFieldFlags = []string{"name", "description", "account", "query", "on-new", "every", "at", "priority", "stop", "max-per-run", "action", "note"}

// ruleFlags holds the field flags of rule create and rule edit.
type ruleFlags struct {
	name, description, query string
	accounts, actions        []string
	onNew, stop              bool
	every, at, note, file    string
	priority, maxPerRun      int32
}

// addRuleFlags registers the field flags and --file on cmd.
func addRuleFlags(cmd *cobra.Command, rf *ruleFlags) {
	f := cmd.Flags()
	f.StringVar(&rf.name, "name", "", "the rule's name: lowercase letters, digits and dashes, starting with a letter or digit")
	f.StringVar(&rf.description, "description", "", "what the rule is for")
	f.StringArrayVar(&rf.accounts, "account", nil, "an account the rule covers (repeatable; default: every account)")
	f.StringVar(&rf.query, "query", "", "the messages to act on, in the search language")
	f.BoolVar(&rf.onNew, "on-new", false, "run on new mail as the cache takes it")
	f.StringVar(&rf.every, "every", "", "run on a schedule, this long after the last run finished: 15m to 30d, such as 6h or 7d")
	f.StringVar(&rf.at, "at", "", "run once a day at this time, HH:MM in the [rules] timezone")
	f.Int32Var(&rf.priority, "priority", 0, "order among rules, lowest first, -1000000 to 1000000 (default 100)")
	f.BoolVar(&rf.stop, "stop", false, "keep later rules off the messages this rule matched")
	f.Int32Var(&rf.maxPerRun, "max-per-run", 0, "the most messages one run acts on, 1 to 5000 (default 500)")
	f.StringArrayVar(&rf.actions, "action", nil, "an action, run in the order given (repeatable): "+ruleActionUsage)
	f.StringVar(&rf.note, "note", "", "the note the forward action adds above the forwarded message")
	f.StringVarP(&rf.file, "file", "f", "", "read the rule from this TOML rule file instead of the field flags")
}

// schedule is the schedule --every or --at gives, "" for neither.
func (rf *ruleFlags) schedule(cmd *cobra.Command) (string, error) {
	switch {
	case rf.every != "" && rf.at != "":
		return "", usageError{err: errors.New("--every and --at cannot be used together"), hint: helpHint(cmd)}
	case rf.every != "":
		return "every:" + rf.every, nil
	case rf.at != "":
		return "at:" + rf.at, nil
	default:
		return "", nil
	}
}

// definition is the RuleDefinition the field flags give. --priority sets
// the priority only when given, so 0 is sent as 0.
func (rf *ruleFlags) definition(cmd *cobra.Command) (*chainmailv1.RuleDefinition, error) {
	schedule, err := rf.schedule(cmd)
	if err != nil {
		return nil, err
	}
	actions, err := parseActions(cmd, rf.actions, rf.note, cmd.Flags().Changed("note"))
	if err != nil {
		return nil, err
	}
	def := &chainmailv1.RuleDefinition{
		Name: rf.name, Description: rf.description, Accounts: rf.accounts, Query: rf.query, OnNew: rf.onNew,
		Schedule: schedule, Stop: rf.stop, Actions: actions, MaxPerRun: rf.maxPerRun,
	}
	if cmd.Flags().Changed("priority") {
		def.Priority = proto.Int32(rf.priority)
	}
	return def, nil
}

// createDefinition is the definition rule create sends: the rule file's
// when --file is given, else the field flags'.
func (rf *ruleFlags) createDefinition(cmd *cobra.Command, disabled bool) (*chainmailv1.RuleDefinition, error) {
	if rf.file != "" {
		if err := fileConflict(cmd, ruleFieldFlags); err != nil {
			return nil, err
		}
		if disabled {
			return nil, usageError{err: errors.New("--file and --disabled cannot be used together: set enabled = false in the file"), hint: helpHint(cmd)}
		}
		def, _, err := readRuleFile(cmd, rf.file)
		return def, err
	}
	def, err := rf.definition(cmd)
	if err != nil {
		return nil, err
	}
	if err := requireRuleFields(cmd, def); err != nil {
		return nil, err
	}
	if disabled {
		def.Enabled = proto.Bool(false)
	}
	return def, nil
}

// fileConflict refuses --file with any of the field flags in names.
func fileConflict(cmd *cobra.Command, names []string) error {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return usageError{err: fmt.Errorf("--file and --%s cannot be used together", name), hint: helpHint(cmd)}
		}
	}
	return nil
}

// requireRuleFields refuses a create with no name, query or action before
// it is sent: the daemon would refuse it too.
func requireRuleFields(cmd *cobra.Command, d *chainmailv1.RuleDefinition) error {
	var missing []string
	if d.GetName() == "" {
		missing = append(missing, "--name")
	}
	if d.GetQuery() == "" {
		missing = append(missing, "--query")
	}
	if len(d.GetActions()) == 0 {
		missing = append(missing, "--action")
	}
	if len(missing) == 0 {
		return nil
	}
	return usageError{err: fmt.Errorf("missing %s: give them, or a rule file with -f", strings.Join(missing, ", ")), hint: helpHint(cmd)}
}

func newRuleCreateCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	rf := &ruleFlags{}
	var disabled bool
	cmd := &cobra.Command{
		Use:   "create (--name NAME --query QUERY --action ACTION... | -f FILE)",
		Short: "Create a rule from flags or a rule file; dry-run it first",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			def, err := rf.createDefinition(cmd, disabled)
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Rules.CreateRule(ctx, connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: def}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, ruleChangedText("created", resp.Msg.GetRule()))
		},
	}
	addRuleFlags(cmd, rf)
	cmd.Flags().BoolVar(&disabled, "disabled", false, "create the rule turned off, to turn on later with rule enable")
	return cmd
}
