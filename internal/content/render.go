package content

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	multiSpace       = regexp.MustCompile(` {2,}`)
	spaceBeforePunct = regexp.MustCompile(` +([,.!?;:])`)
)

// Render returns the template with every {{first_name}} replaced by name. With an empty
// name the placeholder is removed and the text around it tidied: see withoutName.
// lang ("tr" or "en") decides capitalization rules.
func Render(tmpl, firstName, lang string) string {
	if name := strings.TrimSpace(firstName); name != "" {
		return strings.ReplaceAll(tmpl, Placeholder, name)
	}
	return withoutName(tmpl, lang)
}

// withoutName removes the placeholder, line by line:
//
//	"Merhaba {{first_name}}, sepetin seni bekliyor!" → "Merhaba, sepetin seni bekliyor!"
//	"{{first_name}}, sepetindekiler %30 indirimli!"   → "Sepetindekiler %30 indirimli!"
//	"Sevgili {{first_name}}!"                          → "Sevgili!"
//
// Spaces left behind are collapsed, a space before punctuation is dropped, and a line
// that started with the name loses the punctuation after it and is capitalized.
func withoutName(tmpl, lang string) string {
	lines := strings.Split(tmpl, "\n")
	for i, line := range lines {
		if !strings.Contains(line, Placeholder) {
			continue
		}
		atStart := strings.HasPrefix(strings.TrimSpace(line), Placeholder)
		line = strings.ReplaceAll(line, Placeholder, "")
		line = multiSpace.ReplaceAllString(line, " ")
		line = spaceBeforePunct.ReplaceAllString(line, "$1")
		line = strings.TrimSpace(line)
		if atStart {
			line = capitalize(strings.TrimLeft(line, ",;:!. "), lang)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// capitalize upper-cases the first letter with the language's rules: in Turkish "i"
// becomes "İ" and "ı" becomes "I".
func capitalize(s, lang string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	if lang == "tr" {
		r = unicode.TurkishCase.ToUpper(r)
	} else {
		r = unicode.ToUpper(r)
	}
	return string(r) + s[size:]
}

// Rendered is one recipient's messages.
type Rendered struct {
	SMS          string
	EmailSubject string
	EmailBody    string
	// SMSWithoutName / EmailWithoutName report that the recipient gets a version without
	// their name: their first_name is empty, or (SMS and subject only) the text with the
	// name would exceed the channel limit.
	SMSWithoutName   bool
	EmailWithoutName bool
}

// RenderFor renders validated content for one recipient. Validation measured the
// templates with the longest name in the audience, so the limit check here is a safety
// net for data that changed after the audience was counted; a field over its limit with
// the name falls back to the version without it. Only the subject has a limit on the
// email side, so the body keeps the name.
func RenderFor(c Content, firstName, lang string) Rendered {
	hasName := strings.TrimSpace(firstName) != ""
	fit := func(tmpl string, max int) (string, bool) {
		if !strings.Contains(tmpl, Placeholder) {
			return tmpl, false
		}
		if out := Render(tmpl, firstName, lang); hasName && utf8.RuneCountInString(out) <= max {
			return out, false
		}
		return Render(tmpl, "", lang), true
	}
	var r Rendered
	r.SMS, r.SMSWithoutName = fit(c.SMS, SMSMaxRunes)
	var subjectNameless bool
	r.EmailSubject, subjectNameless = fit(c.EmailSubject, SubjectMaxRunes)
	r.EmailBody = Render(c.EmailBody, firstName, lang)
	r.EmailWithoutName = subjectNameless || (!hasName && strings.Contains(c.EmailBody, Placeholder))
	return r
}
