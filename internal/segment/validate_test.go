package segment

import (
	"errors"
	"reflect"
	"testing"
)

var testCities = []string{"İstanbul", "Ankara", "İzmir", "Eskişehir", "Muğla"}

func mustParse(t *testing.T, raw string) Segment {
	t.Helper()
	s, err := NewValidator(testCities).Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse(%s): %v", raw, err)
	}
	return s
}

func TestParseExampleBrief(t *testing.T) {
	s := mustParse(t, `{"rules":[
		{"field":"city","op":"eq","value":"Istanbul"},
		{"field":"cart_added_days_ago","op":"lte","value":30},
		{"field":"purchased_after_cart","op":"eq","value":false}]}`)
	want := []Rule{
		{FieldCity, OpEq, "İstanbul"},
		{FieldCartAddedDaysAgo, OpLte, 30},
		{FieldPurchasedAfterCart, OpEq, false},
	}
	if !reflect.DeepEqual(s.Rules, want) {
		t.Fatalf("rules = %#v, want %#v", s.Rules, want)
	}
}

func TestCityNormalization(t *testing.T) {
	for _, in := range []string{"İstanbul", "Istanbul", "istanbul", "ISTANBUL", "ıstanbul", "  istanbul "} {
		s := mustParse(t, `{"rules":[{"field":"city","op":"eq","value":"`+in+`"}]}`)
		if got := s.Rules[0].Value; got != "İstanbul" {
			t.Errorf("%q → %q, want İstanbul", in, got)
		}
	}
	s := mustParse(t, `{"rules":[{"field":"city","op":"in","value":["eskisehir","MUGLA","Eskişehir"]}]}`)
	if got, want := s.Rules[0].Value, []string{"Eskişehir", "Muğla"}; !reflect.DeepEqual(got, want) {
		t.Errorf("in → %v, want %v (normalized, deduplicated)", got, want)
	}
}

func TestLanguageNormalization(t *testing.T) {
	s := mustParse(t, `{"rules":[{"field":"language","op":"eq","value":"EN"}]}`)
	if s.Rules[0].Value != "en" {
		t.Errorf("got %v, want en", s.Rules[0].Value)
	}
}

func TestDaysRange(t *testing.T) {
	mustParse(t, `{"rules":[
		{"field":"cart_added_days_ago","op":"gte","value":7},
		{"field":"cart_added_days_ago","op":"lte","value":30}]}`)
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, raw  string
		code, path string
	}{
		{"not json", `{"rules":[`, CodeInvalidJSON, "$"},
		{"trailing text", `{"rules":[{"field":"language","op":"eq","value":"tr"}]} sure!`, CodeInvalidJSON, "$"},
		{"unknown top-level key", `{"rules":[{"field":"language","op":"eq","value":"tr"}],"or":[]}`, CodeInvalidJSON, "$"},
		{"unknown rule key", `{"rules":[{"field":"language","op":"eq","value":"tr","not":true}]}`, CodeInvalidJSON, "$"},
		{"missing rules", `{}`, CodeNoRules, "rules"},
		{"empty rules", `{"rules":[]}`, CodeNoRules, "rules"},
		{"unknown field", `{"rules":[{"field":"age","op":"gte","value":18}]}`, CodeUnknownField, "rules[0].field"},
		{"consent field", `{"rules":[{"field":"sms_consent","op":"eq","value":true}]}`, CodeForbiddenField, "rules[0].field"},
		{"contact field", `{"rules":[{"field":"email","op":"eq","value":"a@b.c"}]}`, CodeForbiddenField, "rules[0].field"},
		{"sql in field", `{"rules":[{"field":"city; DROP TABLE users","op":"eq","value":"x"}]}`, CodeUnknownField, "rules[0].field"},
		{"op not allowed", `{"rules":[{"field":"city","op":"like","value":"%ist%"}]}`, CodeOpNotAllowed, "rules[0].op"},
		{"lte on city", `{"rules":[{"field":"city","op":"lte","value":"Ankara"}]}`, CodeOpNotAllowed, "rules[0].op"},
		{"unknown city", `{"rules":[{"field":"city","op":"eq","value":"Paris"}]}`, CodeUnknownCity, "rules[0].value"},
		{"unknown city in list", `{"rules":[{"field":"city","op":"in","value":["Ankara","Paris"]}]}`, CodeUnknownCity, "rules[0].value"},
		{"empty in", `{"rules":[{"field":"city","op":"in","value":[]}]}`, CodeOutOfRange, "rules[0].value"},
		{"in with string", `{"rules":[{"field":"city","op":"in","value":"Ankara"}]}`, CodeWrongType, "rules[0].value"},
		{"eq with array", `{"rules":[{"field":"city","op":"eq","value":["Ankara"]}]}`, CodeWrongType, "rules[0].value"},
		{"null value", `{"rules":[{"field":"purchased_after_cart","op":"eq","value":null}]}`, CodeWrongType, "rules[0].value"},
		{"missing value", `{"rules":[{"field":"purchased_after_cart","op":"eq"}]}`, CodeWrongType, "rules[0].value"},
		{"bool as string", `{"rules":[{"field":"purchased_after_cart","op":"eq","value":"false"}]}`, CodeWrongType, "rules[0].value"},
		{"days as string", `{"rules":[{"field":"cart_added_days_ago","op":"lte","value":"30"}]}`, CodeWrongType, "rules[0].value"},
		{"days fractional", `{"rules":[{"field":"cart_added_days_ago","op":"lte","value":30.5}]}`, CodeWrongType, "rules[0].value"},
		{"days zero", `{"rules":[{"field":"cart_added_days_ago","op":"lte","value":0}]}`, CodeOutOfRange, "rules[0].value"},
		{"days too big", `{"rules":[{"field":"cart_added_days_ago","op":"lte","value":366}]}`, CodeOutOfRange, "rules[0].value"},
		{"unknown language", `{"rules":[{"field":"language","op":"eq","value":"de"}]}`, CodeUnknownLang, "rules[0].value"},
		{"duplicate field", `{"rules":[{"field":"city","op":"eq","value":"Ankara"},{"field":"city","op":"eq","value":"İzmir"}]}`, CodeDuplicateRule, "rules[1]"},
		{"empty range", `{"rules":[{"field":"cart_added_days_ago","op":"gte","value":30},{"field":"cart_added_days_ago","op":"lte","value":7}]}`, CodeEmptyRange, "rules"},
	}
	v := NewValidator(testCities)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Parse([]byte(c.raw))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if len(ve.Errors) != 1 || ve.Errors[0].Code != c.code || ve.Errors[0].Path != c.path {
				t.Fatalf("errors = %+v, want one %s at %s", ve.Errors, c.code, c.path)
			}
		})
	}
}

// All problems are reported at once so one repair round can fix them all.
func TestParseReportsAllErrors(t *testing.T) {
	_, err := NewValidator(testCities).Parse([]byte(`{"rules":[
		{"field":"city","op":"eq","value":"Paris"},
		{"field":"email_consent","op":"eq","value":true},
		{"field":"cart_added_days_ago","op":"lte","value":999}]}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) != 3 {
		t.Fatalf("err = %v, want 3 errors", err)
	}
}

func TestFoldTR(t *testing.T) {
	cases := map[string]string{
		"İstanbul":  "istanbul",
		"IĞDIR":     "igdir",
		"Çanakkale": "canakkale",
		"Şanlıurfa": "sanliurfa",
		"Muğla":     "mugla",
		"Ünye":      "unye",
		"Ödemiş":    "odemis",
		"i̇stanbul": "istanbul", // what strings.ToLower("İstanbul") produces
	}
	for in, want := range cases {
		if got := foldTR(in); got != want {
			t.Errorf("foldTR(%q) = %q, want %q", in, got, want)
		}
	}
}
