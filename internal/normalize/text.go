// Package normalize prepares extracted values for the feed: text cleanup,
// date parsing and (later) body HTML cleanup (§6).
package normalize

import (
	"strings"
	"unicode"
)

// isInvisible reports whether r is one of the zero-width characters that CMS
// "stega" encoding hides in text (§6a).
func isInvisible(r rune) bool {
	return (r >= 0x200B && r <= 0x200D) || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF
}

// StripInvisible removes runs of two or more zero-width characters and any
// lone U+FEFF. A lone character is otherwise kept, because a single U+200D
// (zero-width joiner) is what binds emoji sequences such as "woman technologist" (U+1F469 U+200D U+1F4BB).
func StripInvisible(s string) string {
	if !strings.ContainsFunc(s, isInvisible) {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); {
		if !isInvisible(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && isInvisible(rs[j]) {
			j++
		}
		if j-i == 1 && rs[i] != 0xFEFF {
			b.WriteRune(rs[i])
		}
		i = j
	}
	return b.String()
}

// CleanText prepares a scalar text field: invisible characters stripped,
// whitespace collapsed to single spaces, ends trimmed.
func CleanText(s string) string {
	s = StripInvisible(s)
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}
