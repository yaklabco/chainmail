package render

import (
	"strings"
	"unicode"
)

// CleanText prepares message-derived text for the untrusted envelope: valid
// UTF-8, no control characters other than newline and tab, and no invisible
// characters. Those are the format characters (Unicode category Cf), the
// other default-ignorable code points, the variation selectors and the
// whole U+E0000 to U+E0FFF block: bidirectional controls, zero-width spaces,
// soft hyphens, byte order marks, Hangul fillers, and the tag characters
// that can spell hidden instructions a model reads and a human checking
// what an agent read does not see. Line and paragraph separators become
// newlines.
//
// The zero-width joiner and non-joiner and the emoji and text presentation
// selectors (U+FE0E, U+FE0F) stay, since emoji sequences and scripts such as
// Persian need them, but not right after another invisible character, so a
// run of them collapses and cannot carry a hidden payload. A joiner may
// follow a presentation selector that stayed, as in the rainbow flag.
func CleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	afterInvisible, afterSelector := false, false
	for _, r := range s {
		if joinerOrSelector(r) {
			keep := !afterInvisible || (r == 0x200d && afterSelector)
			if keep {
				b.WriteRune(r)
			}
			afterInvisible, afterSelector = true, keep && (r == 0xfe0e || r == 0xfe0f)
			continue
		}
		c := cleanRune(r)
		if c >= 0 {
			b.WriteRune(c)
		}
		afterInvisible, afterSelector = c < 0, false
	}
	return b.String()
}

// joinerOrSelector reports whether r is ZWNJ, ZWJ or a presentation selector.
func joinerOrSelector(r rune) bool {
	return r == 0x200c || r == 0x200d || r == 0xfe0e || r == 0xfe0f
}

// cleanRune returns what r becomes in clean text, or -1 to drop it.
func cleanRune(r rune) rune {
	switch {
	case r == '\n' || r == '\t':
		return r
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		return -1
	case r == 0x2028 || r == 0x2029:
		return '\n'
	case r >= 0xe0000 && r <= 0xe0fff:
		return -1
	case unicode.In(r, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector):
		return -1
	}
	return r
}

// CleanHeader is CleanText for one-line values such as subjects, names and
// filenames: line breaks and tabs become spaces, and runs of spaces collapse.
func CleanHeader(s string) string {
	return strings.Join(strings.FieldsFunc(CleanText(strings.ReplaceAll(s, "\r", " ")), func(r rune) bool {
		return r == '\n' || r == '\t' || r == ' '
	}), " ")
}
