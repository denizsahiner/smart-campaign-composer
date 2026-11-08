package segment

import (
	"fmt"
	"slices"
	"strings"
)

// RulesSchema returns the JSON Schema for the "rules" array, generated from the whitelist
// so the schema the model is constrained by and the validator can't drift apart. Each
// field is its own anyOf variant, so the schema already ties ops and value types to
// fields.
//
// It sticks to what structured outputs support: no minimum/maximum (days 1–365 are
// checked by the validator) and no enum for cities. A city enum would force the model to
// map an unknown city ("Paris") onto a known one, silently producing a wrong segment;
// as a free string, an unknown city fails validation or goes to unsupported_criteria.
func RulesSchema() map[string]any {
	fields := make([]Field, 0, len(allowedOps))
	for f := range allowedOps {
		fields = append(fields, f)
	}
	slices.Sort(fields)

	var variants []any
	for _, f := range fields {
		for _, op := range allowedOps[f] {
			// Ops that share a value type are merged into one variant (lte/gte).
			if f == FieldCartAddedDaysAgo && op != allowedOps[f][0] {
				continue
			}
			var opSchema map[string]any
			if f == FieldCartAddedDaysAgo {
				opSchema = map[string]any{"type": "string", "enum": allowedOps[f]}
			} else {
				opSchema = map[string]any{"type": "string", "const": op}
			}
			variants = append(variants, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"field": map[string]any{"type": "string", "const": f},
					"op":    opSchema,
					"value": valueSchema(f, op),
				},
				"required":             []string{"field", "op", "value"},
				"additionalProperties": false,
			})
		}
	}
	return map[string]any{
		"type":  "array",
		"items": map[string]any{"anyOf": variants},
	}
}

func valueSchema(f Field, op Op) map[string]any {
	switch {
	case f == FieldCity && op == OpIn:
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case f == FieldCity:
		return map[string]any{"type": "string"}
	case f == FieldLanguage:
		return map[string]any{"type": "string", "enum": Languages}
	case f == FieldCartAddedDaysAgo:
		return map[string]any{"type": "integer"}
	case f == FieldPurchasedAfterCart:
		return map[string]any{"type": "boolean"}
	}
	panic(fmt.Sprintf("segment: no value schema for %s %s", f, op))
}

// Describe returns one plain-English line per rule, for the messages prompt and the UI.
func (s Segment) Describe() []string {
	out := make([]string, 0, len(s.Rules))
	for _, r := range s.Rules {
		var d string
		switch {
		case r.Field == FieldCity && r.Op == OpEq:
			d = "lives in " + r.Value.(string)
		case r.Field == FieldCity && r.Op == OpIn:
			d = "lives in one of: " + strings.Join(r.Value.([]string), ", ")
		case r.Field == FieldLanguage:
			d = "prefers language " + r.Value.(string)
		case r.Field == FieldCartAddedDaysAgo && r.Op == OpLte:
			d = fmt.Sprintf("last added to cart within the last %d days", r.Value.(int))
		case r.Field == FieldCartAddedDaysAgo && r.Op == OpGte:
			d = fmt.Sprintf("last added to cart %d or more days ago", r.Value.(int))
		case r.Field == FieldPurchasedAfterCart && r.Value.(bool):
			d = "bought something after that"
		case r.Field == FieldPurchasedAfterCart:
			d = "has not bought anything since"
		}
		out = append(out, d)
	}
	return out
}
