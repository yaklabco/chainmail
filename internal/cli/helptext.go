package cli

import "github.com/spf13/cobra"

// examples is each command's Example text, by command path. Lines starting
// with # are comments. A test fails for a runnable command without one.
var examples = map[string]string{
	"chainmail": `  # find unread mail, then read a message by its ref
  chainmail search is:unread in:inbox
  chainmail get cm1:work:INBOX:7:42`,
	"chainmail accounts list": `  chainmail accounts list
  chainmail accounts list --output json`,
	"chainmail accounts probe": `  # every account
  chainmail accounts probe
  chainmail accounts probe work gmail`,
	"chainmail accounts test": `  chainmail accounts test work`,
	"chainmail admin status":  `  chainmail admin status`,
	"chainmail archive": `  chainmail archive cm1:work:INBOX:7:42
  # archive everything a search finds
  chainmail search from:newsletter --output json | jq -r '.rows[].ref' | chainmail archive -`,
	"chainmail attachment": `  chainmail attachment cm1:work:INBOX:7:42 2 --out invoice.pdf
  chainmail attachment cm1:work:INBOX:7:42 2 --out - > invoice.pdf`,
	"chainmail audit": `  chainmail audit
  chainmail audit --since 2h --action send
  chainmail audit --account work --limit 20
  # what rules did today
  chainmail audit --client rule`,
	"chainmail auth login":  `  chainmail auth login gmail`,
	"chainmail cache evict": `  chainmail cache evict`,
	"chainmail cache rebuild": `  # rebuild every account whose cache is enabled
  chainmail cache rebuild
  chainmail cache rebuild --account work --folder INBOX`,
	"chainmail cache status": `  chainmail cache status
  chainmail cache status --account work --account gmail --output json`,
	"chainmail copy": `  chainmail copy cm1:work:INBOX:7:42 --to Receipts`,
	"chainmail daemon": `  # production runs under systemd; a development daemon runs as you
  chainmail daemon --dev --config ./config.toml --state-dir ./state --vault-key-file ./vault-key`,
	"chainmail draft create": `  chainmail draft create --account work --to alice@example.com --subject "Notes" --body-file notes.md
  # save a draft, then send it by the ref it prints
  chainmail draft send "$(chainmail draft create --account work --to alice@example.com --subject Hi --body Hi --output json | jq -r .draft.ref)"`,
	"chainmail draft update": `  chainmail draft update cm1:work:Drafts:3:9 --to alice@example.com --subject "Notes, v2" --body-file notes.md`,
	"chainmail draft send":   `  chainmail draft send cm1:work:Drafts:3:9`,
	"chainmail draft delete": `  chainmail draft delete cm1:work:Drafts:3:9`,
	"chainmail expunge":      `  chainmail expunge cm1:work:Trash:5:12`,
	"chainmail flag":         `  chainmail flag cm1:work:INBOX:7:42`,
	"chainmail unflag":       `  chainmail unflag cm1:work:INBOX:7:42`,
	"chainmail read":         `  chainmail read cm1:work:INBOX:7:42 cm1:work:INBOX:7:43`,
	"chainmail unread":       `  chainmail unread cm1:work:INBOX:7:42`,
	"chainmail flags": `  chainmail flags cm1:work:INBOX:7:42 --add '\Flagged' --remove '\Seen'
  chainmail flags cm1:work:INBOX:7:42 --add todo`,
	"chainmail folder create": `  chainmail folder create Projects/2026 --account work`,
	"chainmail folder rename": `  chainmail folder rename Projects/2026 Archive/2026 --account work`,
	"chainmail folder delete": `  chainmail folder delete Old --account work
  # also delete the mail in it
  chainmail folder delete Old --account work --force`,
	"chainmail folders": `  chainmail folders
  chainmail folders --account work`,
	"chainmail forward": `  chainmail forward cm1:work:INBOX:7:42 --to bob@example.com --body "FYI, see below."
  chainmail forward cm1:work:INBOX:7:42 --to bob@example.com --no-attachments --draft`,
	"chainmail get": `  chainmail get cm1:work:INBOX:7:42
  chainmail get --full cm1:work:INBOX:7:42
  chainmail get --raw cm1:work:INBOX:7:42 > message.eml`,
	"chainmail item block": `  chainmail item block it_01jcb7m2k9r8q3v4w5x6y7z8a9 it_01jcb7m2k9r8q3v4w5x6y7z8b0
  chainmail item block it_01jcb7m2k9r8q3v4w5x6y7z8a9 it_01jcb7m2k9r8q3v4w5x6y7z8b0 --remove`,
	"chainmail item create": `  # a task in the inbox, to sort later
  chainmail item create task "Renew the domain" --area work --due 2026-11-20
  # a chore under the work Admin initiative, with acceptance criteria
  chainmail item create task "File the expense report" --parent it_admin_work --criterion "Receipts attached" --criterion "Submitted"
  chainmail item create feature "Search by item" --parent it_01jcb7m2k9r8q3v4w5x6y7z8b0 --description-file spec.md`,
	"chainmail item criteria add":    `  chainmail item criteria add it_01jcb7m2k9r8q3v4w5x6y7z8a9 "Receipts attached" "Submitted"`,
	"chainmail item criteria edit":   `  chainmail item criteria edit it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1 "Every receipt attached"`,
	"chainmail item criteria remove": `  chainmail item criteria remove it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1`,
	"chainmail item criteria order": `  # name every criterion once, in the new order
  chainmail item criteria order it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c2 cr_01jcb7m2k9r8q3v4w5x6y7z8c1`,
	"chainmail item criteria tick":   `  chainmail item criteria tick it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1 --evidence "receipt in cm1:work:INBOX:7:42"`,
	"chainmail item criteria untick": `  chainmail item criteria untick it_01jcb7m2k9r8q3v4w5x6y7z8a9 cr_01jcb7m2k9r8q3v4w5x6y7z8c1`,
	"chainmail item delete": `  chainmail item delete it_01jcb7m2k9r8q3v4w5x6y7z8a9
  # the item and every item below it
  chainmail item delete it_01jcb7m2k9r8q3v4w5x6y7z8b0 --cascade`,
	"chainmail item link": `  chainmail item link it_01jcb7m2k9r8q3v4w5x6y7z8a9 cm1:work:INBOX:7:42 --anchor --note "the renewal notice"
  # then find the item's mail
  chainmail search item:it_01jcb7m2k9r8q3v4w5x6y7z8a9`,
	"chainmail item list": `  # the inbox: tasks not yet sorted
  chainmail item list --inbox
  chainmail item list --under it_admin_work --state in-progress
  # everything changed after revision 120, deleted items included
  chainmail item list --changed-since 120 --output json`,
	"chainmail item move": `  chainmail item move it_01jcb7m2k9r8q3v4w5x6y7z8a9 it_admin_work
  # back to the inbox
  chainmail item move it_01jcb7m2k9r8q3v4w5x6y7z8a9 --root`,
	"chainmail item restore": `  chainmail item restore it_01jcb7m2k9r8q3v4w5x6y7z8a9
  chainmail item restore it_01jcb7m2k9r8q3v4w5x6y7z8b0 --cascade`,
	"chainmail item show": `  chainmail item show it_admin_work
  chainmail item show it_01jcb7m2k9r8q3v4w5x6y7z8a9 --output json`,
	"chainmail item unlink": `  # item show lists each link's account and thread key
  chainmail item unlink it_01jcb7m2k9r8q3v4w5x6y7z8a9 work 18c2f0a1b2c3d4e5`,
	"chainmail item update": `  chainmail item update it_01jcb7m2k9r8q3v4w5x6y7z8a9 --state in-progress --due 2026-11-30
  # drop the due date and the estimate
  chainmail item update it_01jcb7m2k9r8q3v4w5x6y7z8a9 --clear due,estimate`,
	"chainmail label": `  chainmail label cm1:gmail:INBOX:3:17:g1790000000000000001 --add Receipts --remove Todo`,
	"chainmail mcp": `  # configure your agent to run this command over stdio
  chainmail mcp`,
	"chainmail move": `  chainmail move cm1:work:INBOX:7:42 --to Receipts`,
	"chainmail reply": `  chainmail reply cm1:work:INBOX:7:42 --body "Thanks, paid."
  chainmail reply cm1:work:INBOX:7:42 --all --body-file reply.md --draft`,
	"chainmail rule create": `  # file Stripe receipts as they arrive
  chainmail rule create --name receipts --account gmail --query 'from:stripe.com subject:receipt' --on-new --action label=Receipts --action archive
  # archive read inbox mail older than 30 days, once a day
  chainmail rule create --name inbox-cleanup --query 'in:inbox is:read older_than:30d' --every 1d --action archive
  chainmail rule create -f receipts.toml`,
	"chainmail rule delete":  `  chainmail rule delete receipts`,
	"chainmail rule disable": `  chainmail rule disable receipts`,
	"chainmail rule dry-run": `  chainmail rule dry-run receipts
  # an unsaved rule, against the newest cached mail
  chainmail rule dry-run -f receipts.toml --trigger new`,
	"chainmail rule edit": `  chainmail rule edit receipts --priority 10 --stop
  chainmail rule edit inbox-cleanup --at 07:00
  # change only the keys the file holds
  chainmail rule edit receipts -f receipts.toml`,
	"chainmail rule enable": `  chainmail rule enable receipts`,
	"chainmail rule list": `  chainmail rule list
  chainmail rule list --account gmail --disabled`,
	"chainmail rule restore": `  # find the ID of a deleted rule, then restore it by that ID
  chainmail rule list --deleted
  chainmail rule restore ru_01jcb7m2k9r8q3v4w5x6y7z8a9`,
	"chainmail rule run": `  # work through a backlog 100 messages at a time
  chainmail rule run inbox-cleanup --max 100`,
	"chainmail rule runs": `  chainmail rule runs
  chainmail rule runs receipts --outcome failed`,
	"chainmail rule status": `  chainmail rule status`,
	"chainmail rule show": `  chainmail rule show receipts --history
  # save a rule as a file to edit
  chainmail rule show receipts --toml > receipts.toml`,
	"chainmail search": `  chainmail search invoice from:acme newer_than:30d
  chainmail search 'subject:"q3 report" OR has:attachment' --account gmail
  chainmail search is:unread in:inbox --count
  chainmail search from:acme --source server`,
	"chainmail secret set": `  # typed at the terminal without echo
  chainmail secret set accounts/work
  chainmail secret set accounts/work --from-pass mail/work
  chainmail secret set oauth_clients/google --from-pass google/oauth-client`,
	"chainmail send": `  chainmail send --account work --to alice@example.com --subject "Lunch?" --body "Noon at the usual place?"
  chainmail send --account work --to alice@example.com --subject "Report" --body-file report.md --attach report.pdf
  # a plain text body from stdin
  echo "Done." | chainmail send --account work --to alice@example.com --subject "Status" --body - --text
  # see the MIME message without sending it
  chainmail send --account work --to alice@example.com --subject "Test" --body "hi" --dry-run > message.eml`,
	"chainmail send-raw": `  chainmail send-raw --account work --file message.eml
  cat message.eml | chainmail send-raw --account work -`,
	"chainmail thread":  `  chainmail thread cm1:work:INBOX:7:42`,
	"chainmail trash":   `  chainmail trash cm1:work:INBOX:7:42`,
	"chainmail version": `  chainmail version`,
	"chainmail watch": `  # resume where the triage group left off, new mail only
  chainmail watch --group triage --kind new
  # print everything logged for one inbox, then exit
  chainmail watch --since 0 --account work --in inbox --once
  # changes to an item and the items below it, and new mail on their threads
  chainmail watch --kind item,item_mail --item it_01jcb7m2k9r8q3v4w5x6y7z8a9
  # rule changes, and work items coming due
  chainmail watch --kind rule,item_due_soon,item_due,item_overdue`,
	"chainmail watch groups":        `  chainmail watch groups`,
	"chainmail watch groups delete": `  chainmail watch groups delete triage`,
	"chainmail watch status":        `  chainmail watch status`,
}

// longHelp is the Long text of commands whose one-line Short leaves out a
// rule a person needs before running them.
var longHelp = map[string]string{
	"chainmail get": "Read up to 50 messages by ref. Each ref succeeds or fails on its own.\n\n" +
		"--full keeps quoted text and signatures and does not cut the body at body_limit. " +
		"--raw writes one message's original bytes, and refuses a terminal.",
	"chainmail attachment": "Download one attachment by the part number get lists. " +
		"--out names the file to write; --out - writes to stdout, which must not be a terminal.",
}

// applyHelp sets each command's Example and, where given, its Long text.
func applyHelp(root *cobra.Command) {
	walk(root, func(c *cobra.Command) {
		if ex, ok := examples[c.CommandPath()]; ok {
			c.Example = ex
		}
		if long, ok := longHelp[c.CommandPath()]; ok {
			c.Long = long
		}
	})
}
