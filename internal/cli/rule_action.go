package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// ruleActionUsage is the --action grammar, for help and usage errors.
const ruleActionUsage = "label=NAME, unlabel=NAME, copy=FOLDER, move=FOLDER, link_item=ID[,anchor], " +
	"forward=ADDR[,ADDR...], flag, unflag, read, unread, archive or trash"

// ruleActionForm is the form of each --action kind that takes a value.
var ruleActionForm = map[string]string{
	"label": "label=NAME", "unlabel": "unlabel=NAME", "copy": "copy=FOLDER", "move": "move=FOLDER",
	"link_item": "link_item=ID or link_item=ID,anchor", "forward": "forward=ADDR[,ADDR...]",
}

// parseAction parses one --action value. It checks only the grammar: the
// daemon checks names, folders, items, addresses and order.
func parseAction(s string) (*chainmailv1.RuleAction, error) {
	kind, value, hasValue := strings.Cut(s, "=")
	a := &chainmailv1.RuleAction{Kind: kind}
	switch kind {
	case "flag", "unflag", "read", "unread", "archive", "trash":
		if hasValue {
			return nil, fmt.Errorf("--action %s takes no value", kind)
		}
		return a, nil
	case "label", "unlabel", "copy", "move", "link_item", "forward":
	default:
		return nil, fmt.Errorf("unknown --action %q: want %s", s, ruleActionUsage)
	}
	needsValue := fmt.Errorf("--action %s needs a value: %s", kind, ruleActionForm[kind])
	if value == "" {
		return nil, needsValue
	}
	switch kind {
	case "label", "unlabel":
		a.Label = value
	case "copy", "move":
		a.Folder = value
	case "link_item":
		id, modifier, hasModifier := strings.Cut(value, ",")
		switch {
		case id == "":
			return nil, needsValue
		case hasModifier && modifier != "anchor":
			return nil, errors.New("--action link_item takes only ,anchor after the item ID")
		}
		a.ItemId, a.Anchor = id, hasModifier
	case "forward":
		for addr := range strings.SplitSeq(value, ",") {
			addr = strings.TrimSpace(addr)
			if addr == "" {
				return nil, errors.New("--action forward has an empty address")
			}
			a.To = append(a.To, addr)
		}
	}
	return a, nil
}

// parseActions parses the --action values in order and sets note, when
// hasNote, on the first forward. A note with no forward is a usage error.
func parseActions(cmd *cobra.Command, raw []string, note string, hasNote bool) ([]*chainmailv1.RuleAction, error) {
	out := make([]*chainmailv1.RuleAction, 0, len(raw))
	forward := -1
	for _, s := range raw {
		a, err := parseAction(s)
		if err != nil {
			return nil, usageError{err: err, hint: helpHint(cmd)}
		}
		if a.GetKind() == "forward" && forward < 0 {
			forward = len(out)
		}
		out = append(out, a)
	}
	if hasNote {
		if forward < 0 {
			return nil, usageError{err: errors.New("--note applies to a forward action: add --action forward=ADDR"), hint: helpHint(cmd)}
		}
		out[forward].Note = note
	}
	return out, nil
}
