package render

import "testing"

func TestTrimText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		in       string
		fromHTML bool
		want     string
	}{
		{"signature", "Hi\n\n-- \nSam", false, "Hi"},
		{"double dash without space is kept in plain text", "Hi\n--\nSam", false, "Hi\n--\nSam"},
		{"double dash from HTML", "Hi\n--\nSam", true, "Hi"},
		{"interleaved", "> q1\nA1\n> q2\nA2", false, "> q1\nA1\n> q2\nA2"},
		{"quote without attribution is kept", "Hi\n\n> old", false, "Hi\n\n> old"},
		{"two-line attribution", "Yes\n\nOn Tue, 3 Sep 2024 at 09:12, Alex Doe <\nalex@example.com> wrote:\n> q", false, "Yes"},
		{"french", "Oui\n\nLe mar. 3 sept. 2024 à 09:12, Alex Doe <alex@example.com> a écrit :\n> q", false, "Oui"},
		{"spanish", "Sí\n\nEl mar, 3 sept 2024 a las 09:12, Alex Doe (<alex@example.com>) escribió:\n> q", false, "Sí"},
		{"original message", "OK\n\n-----Original Message-----\nFrom: Alex\nSent: Tue\nTo: Sam\nSubject: S\n\nOld", false, "OK"},
		{"outlook header after blank line", "OK\n\nFrom: Alex <a@example.com>\nSent: Tuesday\nTo: Sam\n\nOld", false, "OK"},
		{
			"gmail forward is kept",
			"FYI\n\n---------- Forwarded message ---------\nFrom: Alex <a@example.com>\nDate: Tue\nSubject: S\n\nBody",
			false, "FYI\n\n---------- Forwarded message ---------\nFrom: Alex <a@example.com>\nDate: Tue\nSubject: S\n\nBody",
		},
		{
			"apple forward is kept",
			"FYI\n\nBegin forwarded message:\n\nFrom: Alex <a@example.com>\nDate: Tue\n\nBody",
			false, "FYI\n\nBegin forwarded message:\n\nFrom: Alex <a@example.com>\nDate: Tue\n\nBody",
		},
		{
			"signature above a forward is kept",
			"FYI\n\n-- \nSam\n\n---------- Forwarded message ---------\nFrom: Alex <a@example.com>\nDate: Tue\n\nBody",
			false, "FYI\n\n-- \nSam\n\n---------- Forwarded message ---------\nFrom: Alex <a@example.com>\nDate: Tue\n\nBody",
		},
		{
			"signature above a quoted forward",
			"Thanks\n-- \nSam\n\nOn Tue, 3 Sep 2024, Alex wrote:\n> ---------- Forwarded message ---------\n> Body",
			false, "Thanks",
		},
		{
			"sentences that join into an attribution are kept",
			"Thanks for the notes.\n\nOn reflection I agree.\nHere is what you wrote:\n> the plan",
			false, "Thanks for the notes.\n\nOn reflection I agree.\nHere is what you wrote:\n> the plan",
		},
		{
			"italian forward is kept",
			"FYI\n\nInizio messaggio inoltrato:\n\nDa: Alex <a@example.com>\nData: Tue\n\nCorpo",
			false, "FYI\n\nInizio messaggio inoltrato:\n\nDa: Alex <a@example.com>\nData: Tue\n\nCorpo",
		},
		{
			"signature above a dutch forward is kept",
			"FYI\n\n-- \nSam\n\n---------- Doorgestuurd bericht ---------\nVan: Alex <a@example.com>\nDatum: di\n\nTekst",
			false, "FYI\n\n-- \nSam\n\n---------- Doorgestuurd bericht ---------\nVan: Alex <a@example.com>\nDatum: di\n\nTekst",
		},
		{
			"sentence and attribution-like line are kept",
			"Thanks for the notes.\n\nOn reflection the 3 options look fine.\nHere is what you wrote:\n> the plan",
			false, "Thanks for the notes.\n\nOn reflection the 3 options look fine.\nHere is what you wrote:\n> the plan",
		},
		{
			"attribution-like sentence without a date is kept",
			"Sure.\n\nOn Friday at 3pm, as you wrote:\n> meet at 3pm",
			false, "Sure.\n\nOn Friday at 3pm, as you wrote:\n> meet at 3pm",
		},
		{
			"sentence with a date does not start a wrapped attribution",
			"Noted.\n\nOn 3 Sep 2024 we met.\nAlex wrote:\n> q",
			false, "Noted.\n\nOn 3 Sep 2024 we met.\nAlex wrote:\n> q",
		},
		{
			"signature above interleaved answers is kept",
			"See inline.\n-- \nSam\n\nOn Tue, 3 Sep 2024, Alex wrote:\n> Q1\nA1 mine\n> Q2\nA2 mine",
			false, "See inline.\n-- \nSam\n\nOn Tue, 3 Sep 2024, Alex wrote:\n> Q1\nA1 mine\n> Q2\nA2 mine",
		},
		{"attribution with a 12-hour time", "Yes\n\nOn Tue, Sep 3 at 9:12AM, Alex wrote:\n> q", false, "Yes"},
		{"two-digit year attribution", "Yes\n\nOn 03/09/24, Alex wrote:\n> q", false, "Yes"},
		{
			// A From: block with a Date: line is a pasted header, not history,
			// unless a separator line introduces it.
			"pasted header block is kept",
			"Got this from the bank:\n\nFrom: Bank <x@bank.example>\nDate: Tue, 3 Sep 2024 09:12\nSubject: Verify\n\nClick here.\n\nIs this phishing? What should I do?",
			false, "Got this from the bank:\n\nFrom: Bank <x@bank.example>\nDate: Tue, 3 Sep 2024 09:12\nSubject: Verify\n\nClick here.\n\nIs this phishing? What should I do?",
		},
		{
			"pasted german header block is kept",
			"Schau mal:\n\nVon: Bank <x@bank.example>\nDatum: Di\n\nKlicken.\n\nIst das Phishing?",
			false, "Schau mal:\n\nVon: Bank <x@bank.example>\nDatum: Di\n\nKlicken.\n\nIst das Phishing?",
		},
		{"date after a separator", "OK\n\n________________________________\nFrom: Alex\nDate: Tue\n\nOld", false, "OK"},
		{"datum after original message", "OK\n\n-----Ursprüngliche Nachricht-----\nVon: Alex\nDatum: Di\n\nAlt", false, "OK"},
		{"nothing to trim", "Just text.", false, "Just text."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := trimText(tt.in, tt.fromHTML); got != tt.want {
				t.Errorf("trimText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
