package segment

import (
	"encoding/json"
	"strings"
	"testing"
)

// Keywords structured outputs reject or silently can't enforce; the schema must not rely
// on them. Validation covers those constraints instead.
var unsupportedKeywords = []string{"minimum", "maximum", "minLength", "maxLength", "oneOf", "pattern", "minItems", "maxItems", "multipleOf"}

func TestRulesSchemaUsesSupportedKeywordsOnly(t *testing.T) {
	b, err := json.Marshal(RulesSchema())
	if err != nil {
		t.Fatal(err)
	}
	for _, kw := range unsupportedKeywords {
		if strings.Contains(string(b), `"`+kw+`"`) {
			t.Errorf("schema uses unsupported keyword %q: %s", kw, b)
		}
	}
	if strings.Contains(string(b), "İstanbul") {
		t.Error("cities must not be an enum in the schema")
	}
}

// Every whitelisted field/op appears in the schema, and every variant is an object with
// additionalProperties false (required by structured outputs).
func TestRulesSchemaCoversWhitelist(t *testing.T) {
	variants := RulesSchema()["items"].(map[string]any)["anyOf"].([]any)
	covered := map[string]bool{}
	for _, v := range variants {
		obj := v.(map[string]any)
		if obj["additionalProperties"] != false {
			t.Errorf("variant without additionalProperties false: %v", obj)
		}
		props := obj["properties"].(map[string]any)
		field := props["field"].(map[string]any)["const"].(Field)
		op := props["op"].(map[string]any)
		if c, ok := op["const"]; ok {
			covered[string(field)+"."+string(c.(Op))] = true
		}
		if e, ok := op["enum"]; ok {
			for _, o := range e.([]Op) {
				covered[string(field)+"."+string(o)] = true
			}
		}
	}
	for f, ops := range allowedOps {
		for _, op := range ops {
			if !covered[string(f)+"."+string(op)] {
				t.Errorf("schema doesn't allow %s %s", f, op)
			}
		}
	}
	if n := len(covered); n != 6 {
		t.Errorf("schema allows %d field/op pairs, want 6", n)
	}
}

func TestDescribe(t *testing.T) {
	s := mustParse(t, `{"rules":[
		{"field":"city","op":"eq","value":"istanbul"},
		{"field":"cart_added_days_ago","op":"lte","value":30},
		{"field":"purchased_after_cart","op":"eq","value":false}]}`)
	got := strings.Join(s.Describe(), "; ")
	want := "lives in İstanbul; last added to cart within the last 30 days; has not bought anything since"
	if got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
}
