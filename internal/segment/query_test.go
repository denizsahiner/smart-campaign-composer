package segment

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Nov 7 2026, 14:00 Istanbul.
var testAsOf = time.Date(2026, 11, 7, 14, 0, 0, 0, Location)

func istanbul(y int, m time.Month, d, h, min, sec int) time.Time {
	return time.Date(y, m, d, h, min, sec, 0, Location)
}

func TestBuildExampleBrief(t *testing.T) {
	s := mustParse(t, `{"rules":[
		{"field":"city","op":"eq","value":"istanbul"},
		{"field":"cart_added_days_ago","op":"lte","value":30},
		{"field":"purchased_after_cart","op":"eq","value":false}]}`)
	sql, args, err := Build(s, testAsOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "(u.city = $1) AND " +
		"(" + lastCartAt + " >= $2) AND " +
		"(" + lastCartAt + " IS NOT NULL AND NOT EXISTS (SELECT 1 FROM user_events p WHERE p.user_id = u.id AND p.type = 'purchase' AND p.created_at > " + lastCartAt + "))"
	if sql != want {
		t.Errorf("sql =\n%s\nwant\n%s", sql, want)
	}
	if wantArgs := []any{"İstanbul", istanbul(2026, 10, 8, 0, 0, 0)}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

func TestBuildEachRule(t *testing.T) {
	cases := []struct {
		rule     string
		sql      string
		wantArgs []any
	}{
		{`{"field":"city","op":"in","value":["Ankara","izmir"]}`, "(u.city = ANY($1))", []any{[]string{"Ankara", "İzmir"}}},
		{`{"field":"language","op":"eq","value":"en"}`, "(u.language = $1)", []any{"en"}},
		{`{"field":"cart_added_days_ago","op":"gte","value":60}`, "(" + lastCartAt + " < $1)", []any{istanbul(2026, 9, 9, 0, 0, 0)}},
		{`{"field":"purchased_after_cart","op":"eq","value":true}`, "(EXISTS (SELECT 1 FROM user_events p WHERE p.user_id = u.id AND p.type = 'purchase' AND p.created_at > " + lastCartAt + "))", nil},
	}
	for _, c := range cases {
		s := mustParse(t, `{"rules":[`+c.rule+`]}`)
		sql, args, err := Build(s, testAsOf, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sql != c.sql || !reflect.DeepEqual(args, c.wantArgs) {
			t.Errorf("%s:\n got %s %#v\nwant %s %#v", c.rule, sql, args, c.sql, c.wantArgs)
		}
	}
}

// Placeholders continue after the caller's own parameters.
func TestBuildAppendsArgs(t *testing.T) {
	s := mustParse(t, `{"rules":[{"field":"language","op":"eq","value":"tr"}]}`)
	sql, args, err := Build(s, testAsOf, []any{int64(7)})
	if err != nil {
		t.Fatal(err)
	}
	if sql != "(u.language = $2)" || !reflect.DeepEqual(args, []any{int64(7), "tr"}) {
		t.Fatalf("got %s %#v", sql, args)
	}
}

// Values never end up in the SQL text, even ones that look like SQL.
func TestBuildNeverInterpolatesValues(t *testing.T) {
	v := NewValidator([]string{"x'); DROP TABLE users; --"})
	s, err := v.Parse([]byte(`{"rules":[{"field":"city","op":"eq","value":"x'); DROP TABLE users; --"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := Build(s, testAsOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "DROP") {
		t.Fatalf("value leaked into SQL: %s", sql)
	}
}

func TestBuildRejectsUnvalidatedSegment(t *testing.T) {
	s := Segment{Rules: []Rule{{Field: "1=1 OR city", Op: OpEq, Value: "x"}}}
	if _, _, err := Build(s, testAsOf, nil); !errors.Is(err, ErrNotValidated) {
		t.Fatalf("err = %v, want ErrNotValidated", err)
	}
}

func TestCountSQLAppliesConsentFilter(t *testing.T) {
	s := mustParse(t, `{"rules":[{"field":"language","op":"eq","value":"tr"}]}`)
	sql, _, err := CountSQL(s, testAsOf)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{SMSReachable, EmailReachable} {
		if !strings.Contains(sql, want) {
			t.Errorf("count query missing consent filter %q:\n%s", want, sql)
		}
	}
}

// Every whitelisted field/op pair must have a SQL mapping; catches the whitelist and the
// builder drifting apart.
func TestEveryAllowedOpHasSQL(t *testing.T) {
	values := map[Field]map[Op]string{
		FieldCity:               {OpEq: `"Ankara"`, OpIn: `["Ankara"]`},
		FieldLanguage:           {OpEq: `"tr"`},
		FieldCartAddedDaysAgo:   {OpLte: `10`, OpGte: `10`},
		FieldPurchasedAfterCart: {OpEq: `true`},
	}
	for f, ops := range allowedOps {
		for _, op := range ops {
			val, ok := values[f][op]
			if !ok {
				t.Errorf("no test value for %s %s", f, op)
				continue
			}
			s := mustParse(t, `{"rules":[{"field":"`+string(f)+`","op":"`+string(op)+`","value":`+val+`}]}`)
			if _, _, err := Build(s, testAsOf, nil); err != nil {
				t.Errorf("%s %s: %v", f, op, err)
			}
		}
	}
}
