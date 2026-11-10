package content

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

// CodeUnknownLanguage: the answer has a language that wasn't asked for.
const CodeUnknownLanguage = "unknown_language"

// Set is a campaign's content per language code ("tr", "en"): every recipient gets the
// version in their preferred language.
type Set map[string]Content

// Languages returns the set's language codes in order.
func (s Set) Languages() []string {
	out := make([]string, 0, len(s))
	for l := range s {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

// LanguageOptions are the validation options per language: each language's text is
// measured with the longest first name among the recipients who prefer it.
type LanguageOptions map[string]Options

// SetJSONSchema is the structured-output schema for a set in the given languages: an
// object with exactly those keys, each a Content.
func SetJSONSchema(langs []string) map[string]any {
	props := map[string]any{}
	for _, l := range langs {
		props[l] = JSONSchema()
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             append([]string{}, langs...),
		"additionalProperties": false,
	}
}

// DecodeSet decodes set JSON strictly, without validating the texts: every language in
// langs must be there and no other. It's for showing an invalid draft to the marketer.
func DecodeSet(raw []byte, langs []string) (Set, error) {
	var parts map[string]json.RawMessage
	if err := validation.DecodeStrict(raw, &parts); err != nil {
		return nil, validation.InvalidJSON(err)
	}
	var errs []validation.FieldError
	set := Set{}
	for _, l := range langs {
		part, ok := parts[l]
		if !ok {
			errs = append(errs, validation.FieldError{Path: l, Code: CodeEmpty, Message: "is missing: write the messages in this language"})
			continue
		}
		c, err := Decode(part)
		if err != nil {
			errs = append(errs, prefixed(l, err)...)
			continue
		}
		set[l] = c
	}
	for l := range parts {
		if !slices.Contains(langs, l) {
			errs = append(errs, validation.FieldError{Path: l, Code: CodeUnknownLanguage,
				Message: fmt.Sprintf("unexpected language; write only %s", strings.Join(langs, ", "))})
		}
	}
	if err := validation.OrNil(errs); err != nil {
		return nil, err
	}
	return set, nil
}

// ParseSet decodes and validates untrusted set JSON (the model's messages output) in the
// languages of opts.
func ParseSet(raw []byte, opts LanguageOptions) (Set, error) {
	set, err := DecodeSet(raw, opts.languages())
	if err != nil {
		return nil, err
	}
	return ValidateSet(set, opts)
}

// ValidateSet validates every language of opts, which the set must contain; languages of
// the set that aren't in opts are dropped. Field errors carry the language as a prefix
// ("en.sms"). The set returned is normalized as by Validate.
func ValidateSet(set Set, opts LanguageOptions) (Set, error) {
	var errs []validation.FieldError
	out := Set{}
	for _, l := range opts.languages() {
		c, ok := set[l]
		if !ok {
			errs = append(errs, validation.FieldError{Path: l, Code: CodeEmpty, Message: "is missing: write the messages in this language"})
			continue
		}
		v, err := Validate(c, opts[l])
		if err != nil {
			errs = append(errs, prefixed(l, err)...)
			continue
		}
		out[l] = v
	}
	if err := validation.OrNil(errs); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrorsFor returns the errors of one language from a set's errors, without the prefix.
func ErrorsFor(errs []validation.FieldError, lang string) []validation.FieldError {
	var out []validation.FieldError
	for _, e := range errs {
		if e.Path == lang {
			out = append(out, e)
		} else if rest, ok := strings.CutPrefix(e.Path, lang+"."); ok {
			e.Path = rest
			out = append(out, e)
		}
	}
	return out
}

// Prefix puts lang in front of each error's path ("sms" → "en.sms").
func Prefix(lang string, errs []validation.FieldError) []validation.FieldError {
	out := make([]validation.FieldError, len(errs))
	for i, e := range errs {
		if e.Path == "" || e.Path == "$" { // the language's object as a whole
			e.Path = lang
		} else {
			e.Path = lang + "." + e.Path
		}
		out[i] = e
	}
	return out
}

func prefixed(lang string, err error) []validation.FieldError {
	if ve, ok := err.(*validation.Error); ok {
		return Prefix(lang, ve.Errors)
	}
	return []validation.FieldError{{Path: lang, Code: "invalid", Message: err.Error()}}
}

func (o LanguageOptions) languages() []string {
	out := make([]string, 0, len(o))
	for l := range o {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}
