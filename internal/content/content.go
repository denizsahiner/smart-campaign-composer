// Package content validates campaign message content: SMS text, email subject and email
// body. The same rules run on Claude's draft (Parse) and on the marketer's final, possibly
// edited content at approval (Validate), so neither path can queue something the other
// would reject.
package content

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

// Limits are counted in Unicode code points (runes), not bytes: "ş" is one character
// but two bytes. GSM-7 vs UCS-2 SMS encoding is out of scope (see DECISIONS.md).
const (
	SMSMaxRunes     = 160
	SubjectMaxRunes = 80
	// BodyMaxRunes is a sanity cap, not a channel limit: it stops a runaway generation
	// from being queued thousands of times.
	BodyMaxRunes = 5000
)

// Placeholder is the only personalization variable.
const Placeholder = "{{first_name}}"

const (
	CodeEmpty                = "empty"
	CodeTooLong              = "too_long"
	CodeUnknownPlaceholder   = "unknown_placeholder"
	CodeMalformedPlaceholder = "malformed_placeholder"
	CodeInvalidChar          = "invalid_char"
)

// Content is one campaign's messages. Templates may contain Placeholder.
type Content struct {
	SMS          string `json:"sms"`
	EmailSubject string `json:"email_subject"`
	EmailBody    string `json:"email_body"`
}

type Options struct {
	// LongestFirstName is the length in runes of the longest first_name in the audience.
	// Length limits are checked with a name that long in place of every placeholder, so
	// the template fits every recipient. 0 means the placeholder renders empty.
	LongestFirstName int
}

// Parse decodes and validates untrusted content JSON (the model's messages output).
func Parse(raw []byte, opts Options) (Content, error) {
	var in Content
	if err := validation.DecodeStrict(raw, &in); err != nil {
		return Content{}, validation.InvalidJSON(err)
	}
	// A missing key decodes to "", which Validate reports as empty.
	return Validate(in, opts)
}

// Validate checks typed content and returns it normalized: surrounding whitespace
// trimmed, CRLF turned into LF, and placeholder spelling variants that are unambiguous
// ("{{ First_Name }}") rewritten to Placeholder. On failure the error is a
// *validation.Error listing every problem.
func Validate(c Content, opts Options) (Content, error) {
	fields := []struct {
		path      string
		val       *string
		max       int
		multiline bool
	}{
		{"sms", &c.SMS, SMSMaxRunes, true},
		{"email_subject", &c.EmailSubject, SubjectMaxRunes, false},
		{"email_body", &c.EmailBody, BodyMaxRunes, true},
	}
	var errs []validation.FieldError
	for _, f := range fields {
		*f.val = normalize(*f.val)
		errs = append(errs, check(f.path, *f.val, f.max, f.multiline, opts)...)
	}
	if err := validation.OrNil(errs); err != nil {
		return Content{}, err
	}
	return c, nil
}

// placeholderRe matches anything written in double braces, valid or not.
var placeholderRe = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

func normalize(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.TrimSpace(m[2 : len(m)-2])
		if strings.EqualFold(name, "first_name") {
			return Placeholder
		}
		return m
	})
}

func check(path, s string, max int, multiline bool, opts Options) []validation.FieldError {
	if s == "" {
		return []validation.FieldError{{Path: path, Code: CodeEmpty, Message: "must not be empty"}}
	}
	var errs []validation.FieldError
	fe := func(code, format string, args ...any) {
		errs = append(errs, validation.FieldError{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
	}

	// Unknown names in double braces: {{name}}, {{firstName}}, {{city}}...
	var unknown []string
	for _, m := range placeholderRe.FindAllString(s, -1) {
		if m != Placeholder {
			unknown = append(unknown, m)
		}
	}
	if len(unknown) > 0 {
		fe(CodeUnknownPlaceholder, "unknown placeholder %s; the only supported placeholder is %s",
			strings.Join(unknown, ", "), Placeholder)
	}

	// With every {{...}} removed, any leftover brace or mention of the variable is a
	// malformed placeholder: {first_name}, {{first_name}, %first_name%, [first_name].
	rest := placeholderRe.ReplaceAllString(s, "")
	lower := strings.ToLower(rest)
	if strings.ContainsAny(rest, "{}") || strings.Contains(lower, "first_name") || strings.Contains(lower, "firstname") {
		fe(CodeMalformedPlaceholder, "malformed placeholder or stray brace; write the variable exactly as %s and use no other braces", Placeholder)
	}

	for _, r := range s {
		if r == '\n' && !multiline {
			fe(CodeInvalidChar, "must be a single line")
			break
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			fe(CodeInvalidChar, "contains control character %U", r)
			break
		}
	}

	if n, uses := renderedLen(s, opts.LongestFirstName); n > max {
		if uses > 0 {
			fe(CodeTooLong, "is %d characters with the longest first name in the audience (%d characters) in place of %s; limit %d: shorten by at least %d",
				n, opts.LongestFirstName, Placeholder, max, n-max)
		} else {
			fe(CodeTooLong, "is %d characters; limit %d: shorten by at least %d", n, max, n-max)
		}
	}
	return errs
}

// renderedLen returns the worst-case rendered length of a template in runes and the
// number of placeholders in it.
func renderedLen(s string, nameRunes int) (length, placeholders int) {
	placeholders = strings.Count(s, Placeholder)
	length = utf8.RuneCountInString(s) - placeholders*utf8.RuneCountInString(Placeholder) + placeholders*nameRunes
	return length, placeholders
}
