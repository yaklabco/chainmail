package items

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cleanCase is one input to a Clean function: the text it returns, or the
// Msg of the *InvalidError it refuses with.
type cleanCase struct {
	name    string
	in      string
	want    string
	wantMsg string
}

// runCleanCases checks clean against tests, and every refusal's hint
// against hint.
func runCleanCases(t *testing.T, clean func(string) (string, error), hint string, tests []cleanCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := clean(tt.in)
			if tt.wantMsg == "" {
				if err != nil || got != tt.want {
					t.Errorf("= %q, %v; want %q", shorten(got), err, shorten(tt.want))
				}
				return
			}
			if got != "" {
				t.Errorf("refused text came back as %q", shorten(got))
			}
			if inv := checkInvalid(t, tt.name, err, tt.wantMsg); inv.Hint != hint {
				t.Errorf("Hint = %q, want %q", inv.Hint, hint)
			}
		})
	}
}

// shorten keeps long test strings readable in failure messages.
func shorten(s string) string {
	if len(s) <= 40 {
		return s
	}
	return fmt.Sprintf("%s... (%d bytes)", s[:40], len(s))
}

func TestCleanTitle(t *testing.T) {
	t.Parallel()
	const tooLong = "the title is longer than 200 characters"
	runCleanCases(t, CleanTitle, "a title is 1 to 200 characters", []cleanCase{
		{name: "plain", in: "Renew passport", want: "Renew passport"},
		{name: "trimmed at both ends", in: " \t Renew passport \n", want: "Renew passport"},
		{name: "inner space kept", in: "Renew  passport", want: "Renew  passport"},
		{name: "200 characters", in: strings.Repeat("a", 200), want: strings.Repeat("a", 200)},
		{name: "200 two-byte characters", in: strings.Repeat("é", 200), want: strings.Repeat("é", 200)},
		{name: "200 characters after trimming", in: "  " + strings.Repeat("a", 200) + "  ", want: strings.Repeat("a", 200)},
		{name: "201 characters", in: strings.Repeat("a", 201), wantMsg: tooLong},
		{name: "201 two-byte characters", in: strings.Repeat("é", 201), wantMsg: tooLong},
		{name: "empty", in: "", wantMsg: "the title is empty"},
		{name: "only space", in: " \t\n ", wantMsg: "the title is empty"},
		{name: "not UTF-8", in: "bad \xff byte", wantMsg: "the title is not valid UTF-8"},
	})
}

func TestCleanDescription(t *testing.T) {
	t.Parallel()
	const tooLong = "the description is longer than 65536 bytes"
	full := strings.Repeat("a", 65536)
	runCleanCases(t, CleanDescription, "a description is at most 65536 bytes", []cleanCase{
		{name: "empty", in: "", want: ""},
		{name: "only space", in: " \n\t", want: ""},
		{name: "leading indentation kept", in: "    code\n- item\n\n", want: "    code\n- item"},
		{name: "65536 bytes", in: full, want: full},
		{name: "65536 bytes after trimming", in: full + " \n", want: full},
		{name: "65537 bytes", in: full + "a", wantMsg: tooLong},
		{name: "32768 two-byte characters", in: strings.Repeat("é", 32768), want: strings.Repeat("é", 32768)},
		{name: "32769 two-byte characters", in: strings.Repeat("é", 32769), wantMsg: tooLong},
		{name: "not UTF-8", in: "\xff", wantMsg: "the description is not valid UTF-8"},
	})
}

func TestCleanCriterion(t *testing.T) {
	t.Parallel()
	const tooLong = "the criterion is longer than 500 characters"
	runCleanCases(t, CleanCriterion, "a criterion is 1 to 500 characters", []cleanCase{
		{name: "plain", in: "Form posted", want: "Form posted"},
		{name: "trimmed", in: "\tForm posted \n", want: "Form posted"},
		{name: "500 characters", in: strings.Repeat("a", 500), want: strings.Repeat("a", 500)},
		{name: "500 two-byte characters", in: strings.Repeat("é", 500), want: strings.Repeat("é", 500)},
		{name: "501 characters", in: strings.Repeat("a", 501), wantMsg: tooLong},
		{name: "empty", in: "", wantMsg: "the criterion is empty"},
		{name: "only space", in: "  ", wantMsg: "the criterion is empty"},
		{name: "not UTF-8", in: "\xc3", wantMsg: "the criterion is not valid UTF-8"},
	})
}

func TestCleanEvidence(t *testing.T) {
	t.Parallel()
	const tooLong = "the evidence is longer than 1000 characters"
	runCleanCases(t, CleanEvidence, "ticking a criterion needs evidence of 1 to 1000 characters", []cleanCase{
		{name: "plain", in: "Receipt 4411", want: "Receipt 4411"},
		{name: "trimmed", in: " Receipt 4411\n", want: "Receipt 4411"},
		{name: "1000 characters", in: strings.Repeat("a", 1000), want: strings.Repeat("a", 1000)},
		{name: "1000 two-byte characters", in: strings.Repeat("é", 1000), want: strings.Repeat("é", 1000)},
		{name: "1001 characters", in: strings.Repeat("a", 1001), wantMsg: tooLong},
		{name: "empty", in: "", wantMsg: "the evidence is empty"},
		{name: "only space", in: "\n", wantMsg: "the evidence is empty"},
		{name: "not UTF-8", in: "\xff", wantMsg: "the evidence is not valid UTF-8"},
	})
}

func TestCleanNote(t *testing.T) {
	t.Parallel()
	runCleanCases(t, CleanNote, "a note is at most 500 characters", []cleanCase{
		{name: "empty", in: "", want: ""},
		{name: "only space", in: "  ", want: ""},
		{name: "plain", in: " the quote thread \n", want: "the quote thread"},
		{name: "500 characters", in: strings.Repeat("a", 500), want: strings.Repeat("a", 500)},
		{name: "500 two-byte characters", in: strings.Repeat("é", 500), want: strings.Repeat("é", 500)},
		{name: "501 characters", in: strings.Repeat("a", 501), wantMsg: "the note is longer than 500 characters"},
		{name: "not UTF-8", in: "\xff", wantMsg: "the note is not valid UTF-8"},
	})
}

// However long the text, a refusal never repeats it.
func TestCleanErrorsHoldNoText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat(modelMarker+" ", MaxDescription/len(modelMarker)+1)
	cleaners := []struct {
		name  string
		clean func(string) (string, error)
	}{
		{"CleanTitle", CleanTitle},
		{"CleanDescription", CleanDescription},
		{"CleanCriterion", CleanCriterion},
		{"CleanEvidence", CleanEvidence},
		{"CleanNote", CleanNote},
	}
	for _, c := range cleaners {
		_, err := c.clean(long)
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("%s(%d bytes) = %v, want an *InvalidError", c.name, len(long), err)
			continue
		}
		if strings.Contains(inv.Error(), modelMarker) || strings.Contains(inv.Hint, modelMarker) {
			t.Errorf("%s refusal %q with hint %q holds the text", c.name, inv.Error(), inv.Hint)
		}
	}
}

func TestLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		got, want int
	}{
		{"MaxTitle", MaxTitle, 200},
		{"MaxDescription", MaxDescription, 65536},
		{"MaxCriterion", MaxCriterion, 500},
		{"MaxEvidence", MaxEvidence, 1000},
		{"MaxNote", MaxNote, 500},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
	if RequestRetention != 7*24*time.Hour {
		t.Errorf("RequestRetention = %v, want 168h", RequestRetention)
	}
}

func TestValidKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want bool
	}{
		{"", false},
		{"a", true},
		{"mcp-ABCDEFGHIJ234567", true},
		{"A-z_0.9:x", true},
		{strings.Repeat("k", 128), true},
		{strings.Repeat("k", 129), false},
		{"has space", false},
		{"slash/no", false},
		{"plus+no", false},
		{"tab\tno", false},
		{"newline\n", false},
		{"café", false},
	}
	for _, tt := range tests {
		if got := ValidKey(tt.key); got != tt.want {
			t.Errorf("ValidKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}
