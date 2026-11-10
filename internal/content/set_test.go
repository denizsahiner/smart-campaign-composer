package content

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

const trJSON = `{"sms":"Merhaba {{first_name}}!","email_subject":"Konu","email_body":"Gövde"}`
const enJSON = `{"sms":"Hi {{first_name}}!","email_subject":"Subject","email_body":"Body"}`

func paths(err error) string {
	var ve *validation.Error
	if !errors.As(err, &ve) {
		return fmt.Sprintf("not a validation error: %v", err)
	}
	var out []string
	for _, e := range ve.Errors {
		out = append(out, e.Path+":"+e.Code)
	}
	return strings.Join(out, " ")
}

func TestParseSet(t *testing.T) {
	opts := LanguageOptions{"tr": {LongestFirstName: 5}, "en": {LongestFirstName: 5}}
	set, err := ParseSet([]byte(`{"tr":`+trJSON+`,"en":`+enJSON+`}`), opts)
	if err != nil {
		t.Fatal(err)
	}
	if set["tr"].SMS != "Merhaba {{first_name}}!" || set["en"].EmailSubject != "Subject" {
		t.Errorf("set = %+v", set)
	}
	if got := set.Languages(); fmt.Sprint(got) != "[en tr]" {
		t.Errorf("Languages = %v", got)
	}
}

func TestParseSetErrorsCarryTheLanguage(t *testing.T) {
	opts := LanguageOptions{"tr": {}, "en": {}}
	for name, tc := range map[string]struct{ raw, want string }{
		"missing language": {`{"tr":` + trJSON + `}`, "en:empty"},
		"extra language":   {`{"tr":` + trJSON + `,"en":` + enJSON + `,"de":` + enJSON + `}`, "de:unknown_language"},
		"bad field":        {`{"tr":` + trJSON + `,"en":{"sms":"Hi {{name}}","email_subject":"S","email_body":""}}`, "en.sms:unknown_placeholder en.email_body:empty"},
		"unknown key":      {`{"tr":` + trJSON + `,"en":{"sms":"x","email_subject":"S","email_body":"B","html":"x"}}`, "en:invalid_json"},
		"not an object":    {`[]`, "$:invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSet([]byte(tc.raw), opts)
			if got := paths(err); got != tc.want {
				t.Errorf("errors = %s, want %s", got, tc.want)
			}
		})
	}
}

// Each language is measured with its own longest name.
func TestValidateSetUsesEachLanguagesLongestName(t *testing.T) {
	sms := "Hi {{first_name}}, " + strings.Repeat("x", 140) // 144 + name
	set := Set{"tr": {SMS: sms, EmailSubject: "S", EmailBody: "B"}, "en": {SMS: sms, EmailSubject: "S", EmailBody: "B"}}
	_, err := ValidateSet(set, LanguageOptions{"tr": {LongestFirstName: 20}, "en": {LongestFirstName: 10}})
	if got := paths(err); got != "tr.sms:too_long" {
		t.Errorf("errors = %s, want only tr.sms too long", got)
	}
}

func TestValidateSetDropsLanguagesNotAsked(t *testing.T) {
	c := Content{SMS: " Merhaba ", EmailSubject: "S", EmailBody: "B"}
	out, err := ValidateSet(Set{"tr": c, "en": c}, LanguageOptions{"tr": {}})
	if err != nil || len(out) != 1 || out["tr"].SMS != "Merhaba" {
		t.Errorf("out = %+v, err = %v", out, err)
	}
}

func TestErrorsForAndPrefix(t *testing.T) {
	errs := []validation.FieldError{{Path: "tr.sms", Code: "a"}, {Path: "en", Code: "b"}, {Path: "en.email_body", Code: "c"}}
	if got := ErrorsFor(errs, "en"); len(got) != 2 || got[0].Path != "en" || got[1].Path != "email_body" {
		t.Errorf("ErrorsFor = %+v", got)
	}
	if got := Prefix("en", []validation.FieldError{{Path: "sms"}, {Path: "$"}}); got[0].Path != "en.sms" || got[1].Path != "en" {
		t.Errorf("Prefix = %+v", got)
	}
}

func TestSetJSONSchema(t *testing.T) {
	s := SetJSONSchema([]string{"en"})
	if fmt.Sprint(s["required"]) != "[en]" || s["properties"].(map[string]any)["en"] == nil {
		t.Errorf("schema = %v", s)
	}
}
