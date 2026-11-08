// Package segment holds the audience DSL: the whitelist of fields and operators, the
// validator that turns untrusted (LLM) JSON into a typed Segment, and the query builder
// that turns a validated Segment into parameterized SQL.
//
// Nothing outside this package builds SQL from a segment, and Build refuses a Segment that
// didn't come out of Validator.Parse, so LLM output can't reach the database unchecked.
// A segment loaded back from campaigns.segment_rule is parsed again for the same reason.
package segment

type Field string

const (
	FieldCity               Field = "city"
	FieldLanguage           Field = "language"
	FieldCartAddedDaysAgo   Field = "cart_added_days_ago"
	FieldPurchasedAfterCart Field = "purchased_after_cart"
)

type Op string

const (
	OpEq  Op = "eq"
	OpIn  Op = "in"
	OpLte Op = "lte"
	OpGte Op = "gte"
)

const (
	MinDays = 1
	MaxDays = 365
	// MaxInValues caps the list of an "in" rule; there are only a dozen known cities.
	MaxInValues = 20
)

// Languages are the values allowed for FieldLanguage (mirrors the users.language CHECK).
var Languages = []string{"tr", "en"}

// allowedOps is the whitelist: a field not in this map, or an op not listed for it,
// fails validation.
var allowedOps = map[Field][]Op{
	FieldCity:               {OpEq, OpIn},
	FieldLanguage:           {OpEq},
	FieldCartAddedDaysAgo:   {OpLte, OpGte},
	FieldPurchasedAfterCart: {OpEq},
}

// forbiddenFields exist on users but are deliberately not segment criteria. They get a
// specific error message so a repair prompt tells Claude why, not just "unknown field".
var forbiddenFields = map[string]string{
	"sms_consent":   "consent is applied automatically per channel; remove this rule",
	"email_consent": "consent is applied automatically per channel; remove this rule",
	"first_name":    "contact fields are not segment criteria; remove this rule",
	"last_name":     "contact fields are not segment criteria; remove this rule",
	"email":         "contact fields are not segment criteria; remove this rule",
	"phone":         "contact fields are not segment criteria; remove this rule",
}

// Rule is one validated condition. Value has a concrete type that depends on the field
// and op: string (city eq, language eq), []string (city in), int (cart_added_days_ago),
// bool (purchased_after_cart).
type Rule struct {
	Field Field `json:"field"`
	Op    Op    `json:"op"`
	Value any   `json:"value"`
}

// Segment is a validated audience definition. All rules are combined with AND.
// Obtain one from Validator.Parse; a hand-built Segment is rejected by Build.
type Segment struct {
	Rules []Rule `json:"rules"`

	validated bool
}
