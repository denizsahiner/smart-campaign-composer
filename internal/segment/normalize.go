package segment

import (
	"strings"
	"unicode"
)

// foldTR returns a comparison key that ignores case and Turkish diacritics, so "Istanbul",
// "istanbul", "ISTANBUL" and "İstanbul" all map to "istanbul".
//
// strings.ToLower is not enough: it maps "İ" to "i" + U+0307 (combining dot above), and
// leaves "I" → "i" (in Turkish it's "ı"). Folding every i-variant to plain "i" and
// dropping the combining dot sidesteps locale rules entirely.
func foldTR(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch r {
		case 'İ', 'I', 'ı', 'i':
			r = 'i'
		case 'Ş', 'ş':
			r = 's'
		case 'Ğ', 'ğ':
			r = 'g'
		case 'Ç', 'ç':
			r = 'c'
		case 'Ö', 'ö':
			r = 'o'
		case 'Ü', 'ü':
			r = 'u'
		case '̇':
			continue
		default:
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
