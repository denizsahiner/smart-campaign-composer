package segment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

// Error codes. They're stable so tests, llm_calls.validation_errors and the UI can rely
// on them; the message is the human/LLM-readable part used in repair prompts.
const (
	CodeNoRules        = "no_rules"
	CodeUnknownField   = "unknown_field"
	CodeForbiddenField = "forbidden_field"
	CodeOpNotAllowed   = "op_not_allowed"
	CodeWrongType      = "wrong_type"
	CodeOutOfRange     = "out_of_range"
	CodeUnknownCity    = "unknown_city"
	CodeUnknownLang    = "unknown_language"
	CodeDuplicateRule  = "duplicate_rule"
	CodeEmptyRange     = "empty_range"
)

// FieldError is the shared error shape; see package validation.
type FieldError = validation.FieldError

// Validator checks segment JSON against the whitelist and the known city list.
type Validator struct {
	cities   map[string]string // folded name → canonical name
	cityList []string          // canonical names, for error messages
}

// NewValidator returns a validator that accepts only the given cities (normally the
// distinct users.city values). Matching ignores case and Turkish diacritics.
func NewValidator(cities []string) *Validator {
	v := &Validator{cities: make(map[string]string, len(cities))}
	for _, c := range cities {
		if _, dup := v.cities[foldTR(c)]; !dup {
			v.cities[foldTR(c)] = c
			v.cityList = append(v.cityList, c)
		}
	}
	slices.Sort(v.cityList)
	return v
}

// Cities returns the canonical known cities, sorted; the prompt lists them for Claude.
func (v *Validator) Cities() []string { return slices.Clone(v.cityList) }

type rawSegment struct {
	Rules *[]rawRule `json:"rules"`
}

type rawRule struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

// Parse decodes and validates untrusted segment JSON. On success the returned Segment has
// normalized values (canonical city names, deduplicated "in" lists) and is accepted by
// Build. On failure the error is a *validation.Error.
func (v *Validator) Parse(raw []byte) (Segment, error) {
	var in rawSegment
	if err := validation.DecodeStrict(raw, &in); err != nil {
		return Segment{}, validation.InvalidJSON(err)
	}
	if in.Rules == nil || len(*in.Rules) == 0 {
		// An empty rule list would target every user. That's never what a brief means,
		// and it's the failure mode of a model that couldn't map the brief, so it's
		// rejected rather than silently becoming "send to everyone".
		return Segment{}, &validation.Error{Errors: []FieldError{{
			Path: "rules", Code: CodeNoRules, Message: "at least one rule is required",
		}}}
	}

	var errs []FieldError
	seg := Segment{Rules: make([]Rule, 0, len(*in.Rules))}
	for i, rr := range *in.Rules {
		path := fmt.Sprintf("rules[%d]", i)
		r, fe := v.parseRule(path, rr)
		if fe != nil {
			errs = append(errs, *fe)
			continue
		}
		seg.Rules = append(seg.Rules, r)
	}
	errs = append(errs, crossRuleErrors(seg.Rules)...)
	if err := validation.OrNil(errs); err != nil {
		return Segment{}, err
	}
	seg.validated = true
	return seg, nil
}

func (v *Validator) parseRule(path string, rr rawRule) (Rule, *FieldError) {
	field := Field(rr.Field)
	ops, ok := allowedOps[field]
	if !ok {
		if why, forbidden := forbiddenFields[rr.Field]; forbidden {
			return Rule{}, fieldErr(path+".field", CodeForbiddenField,
				fmt.Sprintf("field %q is not allowed: %s", rr.Field, why))
		}
		return Rule{}, fieldErr(path+".field", CodeUnknownField,
			fmt.Sprintf("unknown field %q; allowed fields: %s", rr.Field, fieldList()))
	}
	op := Op(rr.Op)
	if !slices.Contains(ops, op) {
		return Rule{}, fieldErr(path+".op", CodeOpNotAllowed,
			fmt.Sprintf("op %q is not allowed for field %q; allowed: %s", rr.Op, field, joinOps(ops)))
	}

	vpath := path + ".value"
	r := Rule{Field: field, Op: op}
	switch {
	case field == FieldCity && op == OpEq:
		var s string
		if !decodeValue(rr.Value, &s) {
			return Rule{}, wrongType(vpath, "a string")
		}
		c, ok := v.cities[foldTR(s)]
		if !ok {
			return Rule{}, v.unknownCity(vpath, s)
		}
		r.Value = c

	case field == FieldCity && op == OpIn:
		var ss []string
		if !decodeValue(rr.Value, &ss) || ss == nil {
			return Rule{}, wrongType(vpath, "an array of strings")
		}
		if len(ss) == 0 || len(ss) > MaxInValues {
			return Rule{}, fieldErr(vpath, CodeOutOfRange,
				fmt.Sprintf("must contain 1-%d cities, got %d", MaxInValues, len(ss)))
		}
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			c, ok := v.cities[foldTR(s)]
			if !ok {
				return Rule{}, v.unknownCity(vpath, s)
			}
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		r.Value = out

	case field == FieldLanguage:
		var s string
		if !decodeValue(rr.Value, &s) {
			return Rule{}, wrongType(vpath, "a string")
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if !slices.Contains(Languages, s) {
			return Rule{}, fieldErr(vpath, CodeUnknownLang,
				fmt.Sprintf("unknown language %q; allowed: %s", s, strings.Join(Languages, ", ")))
		}
		r.Value = s

	case field == FieldCartAddedDaysAgo:
		// Decoded into `any` with UseNumber, then type-checked: decoding straight into
		// json.Number would accept the string "30", and float64 would accept 30.5.
		var x any
		if !decodeValue(rr.Value, &x) {
			return Rule{}, wrongType(vpath, "an integer")
		}
		n, isNum := x.(json.Number)
		if !isNum {
			return Rule{}, wrongType(vpath, "an integer")
		}
		days, err := n.Int64() // fails for 30.5 and 1e2
		if err != nil {
			return Rule{}, wrongType(vpath, "an integer")
		}
		if days < MinDays || days > MaxDays {
			return Rule{}, fieldErr(vpath, CodeOutOfRange,
				fmt.Sprintf("must be between %d and %d, got %d", MinDays, MaxDays, days))
		}
		r.Value = int(days)

	case field == FieldPurchasedAfterCart:
		var b bool
		if !decodeValue(rr.Value, &b) {
			return Rule{}, wrongType(vpath, "a boolean")
		}
		r.Value = b
	}
	return r, nil
}

// crossRuleErrors catches combinations that are each valid but together are ambiguous or
// can never match. Each field may appear once, except cart_added_days_ago, which may have
// one lte and one gte to form a range.
func crossRuleErrors(rules []Rule) []FieldError {
	var errs []FieldError
	seen := map[string]int{}
	var lte, gte *int
	for i, r := range rules {
		key := string(r.Field)
		if r.Field == FieldCartAddedDaysAgo {
			key += "." + string(r.Op)
			d := r.Value.(int)
			if r.Op == OpLte {
				lte = &d
			} else {
				gte = &d
			}
		}
		if first, dup := seen[key]; dup {
			errs = append(errs, *fieldErr(fmt.Sprintf("rules[%d]", i), CodeDuplicateRule,
				fmt.Sprintf("duplicates rules[%d]; use a single rule per field (for several cities use op \"in\")", first)))
			continue
		}
		seen[key] = i
	}
	if lte != nil && gte != nil && *gte > *lte {
		errs = append(errs, *fieldErr("rules", CodeEmptyRange,
			fmt.Sprintf("cart_added_days_ago gte %d and lte %d can never both be true", *gte, *lte)))
	}
	return errs
}

func (v *Validator) unknownCity(path, got string) *FieldError {
	return fieldErr(path, CodeUnknownCity,
		fmt.Sprintf("unknown city %q; known cities: %s", got, strings.Join(v.cityList, ", ")))
}

func fieldErr(path, code, msg string) *FieldError {
	return &FieldError{Path: path, Code: code, Message: msg}
}

func wrongType(path, want string) *FieldError {
	return fieldErr(path, CodeWrongType, "must be "+want)
}

// decodeValue decodes a rule value. JSON null and type mismatches both report false
// (null would otherwise decode to the zero value: "", 0, false).
func decodeValue(raw json.RawMessage, dst any) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(dst) == nil
}

func fieldList() string {
	fs := make([]string, 0, len(allowedOps))
	for f := range allowedOps {
		fs = append(fs, string(f))
	}
	slices.Sort(fs)
	return strings.Join(fs, ", ")
}

func joinOps(ops []Op) string {
	s := make([]string, len(ops))
	for i, o := range ops {
		s[i] = string(o)
	}
	return strings.Join(s, ", ")
}
