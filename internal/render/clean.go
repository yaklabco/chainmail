package render

import (
	"strings"
	"unicode"
)

// CleanText prepares message-derived text for the untrusted envelope: valid
// UTF-8, no control characters other than newline and tab, and no format
// characters (Unicode category Cf). Those are invisible: bidirectional
// controls, zero-width spaces, soft hyphens, byte order marks, and the tag
// characters that can spell hidden instructions a model reads and a human
// checking what an agent read does not see. The zero-width joiner and
// non-joiner stay, since emoji sequences and scripts such as Persian need
// them. Variation selectors go too, since a run of them can carry a hidden
// payload, except the two that pick emoji or text presentation. Line and
// paragraph separators become newlines.
func CleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		case r == 0x2028 || r == 0x2029:
			return '\n'
		case r == 0x200c || r == 0x200d:
			return r
		case r >= 0xfe00 && r <= 0xfe0d, r >= 0xe0100 && r <= 0xe01ef:
			return -1
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// CleanHeader is CleanText for one-line values such as subjects, names and
// filenames: line breaks and tabs become spaces, and runs of spaces collapse.
func CleanHeader(s string) string {
	return strings.Join(strings.FieldsFunc(CleanText(strings.ReplaceAll(s, "\r", " ")), func(r rune) bool {
		return r == '\n' || r == '\t' || r == ' '
	}), " ")
}
