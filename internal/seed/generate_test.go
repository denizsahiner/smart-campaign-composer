package seed

import (
	"reflect"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
)

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func TestGenerateIsDeterministic(t *testing.T) {
	opts := Options{Users: 500, Seed: 42, Now: testNow}
	u1, e1 := Generate(opts)
	u2, e2 := Generate(opts)
	if !reflect.DeepEqual(u1, u2) || !reflect.DeepEqual(e1, e2) {
		t.Fatal("same options produced different data")
	}
}

func TestGenerateUserCount(t *testing.T) {
	users, _ := Generate(Options{Users: 5000, Seed: 42, Now: testNow})
	if len(users) != 5000 {
		t.Fatalf("got %d users, want 5000", len(users))
	}
	for i, u := range users {
		if u.ID != int64(i+1) {
			t.Fatalf("user at index %d has ID %d, want sequential IDs", i, u.ID)
		}
	}
}

func TestEventsWithinUserLifetime(t *testing.T) {
	users, events := Generate(Options{Users: 5000, Seed: 42, Now: testNow})
	for _, e := range events {
		u := users[e.UserID-1]
		if e.CreatedAt.Before(u.CreatedAt) || e.CreatedAt.After(testNow) {
			t.Fatalf("event %+v outside user %d lifetime [%v, %v]", e, u.ID, u.CreatedAt, testNow)
		}
	}
}

func TestEmailsUnique(t *testing.T) {
	users, _ := Generate(Options{Users: 5000, Seed: 42, Now: testNow})
	seen := map[string]bool{}
	for _, u := range users {
		if u.Email == nil {
			continue
		}
		if seen[*u.Email] {
			t.Fatalf("duplicate email %q", *u.Email)
		}
		seen[*u.Email] = true
	}
}

// The example brief ("İstanbul, added to cart in the last 30 days, didn't buy") must
// reach a meaningful number of users.
func TestExampleBriefReach(t *testing.T) {
	users, events := Generate(Options{Users: 5000, Seed: 42, Now: testNow})
	n := len(abandonedCarts(users, events, "İstanbul", 30))
	if n < 400 || n > 650 {
		t.Fatalf("example brief matches %d users, want 400-650", n)
	}
}

func TestEdgeCases(t *testing.T) {
	users, events := Generate(Options{Users: 50, Seed: 42, Now: testNow})
	byName := map[string]User{}
	for _, u := range users[:11] {
		byName[u.FirstName+" "+u.LastName] = u
	}
	matched := abandonedCarts(users, events, "İstanbul", 30)

	cases := []struct {
		name      string
		wantMatch bool
		check     func(User) bool
	}{
		{"Abdurrahmangazi Karamustafaoğlu", true, func(u User) bool { return utf8.RuneCountInString(u.FirstName) >= 15 }},
		{" Yılmaz", true, func(u User) bool { return u.FirstName == "" }},
		{"Selin Aydın", true, nil},  // 29 days ago
		{"Kerem Aydın", false, nil}, // 31 days ago
		{"Deniz Kaya", false, nil},  // bought after cart add
		{"Ozan Demir", true, nil},   // bought before last cart add
		{"Ceren Polat", true, func(u User) bool { return u.SMSConsent && u.Phone == nil }},
		{"Berk Koç", true, func(u User) bool { return u.EmailConsent && u.Email == nil }},
		{"Nazlı Şen", true, func(u User) bool { return !u.SMSConsent && !u.EmailConsent }},
		{"Emre Çetin", true, func(u User) bool { return u.SMSConsent && *u.Phone == "+902125550123" }},
		{"Gizem Arslan", true, func(u User) bool { return u.EmailConsent && *u.Email == "gizem.arslan@bounce.invalid" }},
	}
	for _, c := range cases {
		u, ok := byName[c.name]
		if !ok {
			t.Errorf("edge case %q missing", c.name)
			continue
		}
		if matched[u.ID] != c.wantMatch {
			t.Errorf("%q: example brief match = %v, want %v", c.name, matched[u.ID], c.wantMatch)
		}
		if c.check != nil && !c.check(u) {
			t.Errorf("%q: field check failed: %+v", c.name, u)
		}
	}
}

// abandonedCarts mirrors the DSL semantics: the most recent cart_add is at most `days`
// calendar days ago (segment.DaysAgo) and no purchase happened after it.
func abandonedCarts(users []User, events []Event, city string, days int) map[int64]bool {
	lastCart := map[int64]time.Time{}
	for _, e := range events {
		if e.Type == EventCartAdd && e.CreatedAt.After(lastCart[e.UserID]) {
			lastCart[e.UserID] = e.CreatedAt
		}
	}
	boughtAfter := map[int64]bool{}
	for _, e := range events {
		if c, ok := lastCart[e.UserID]; ok && e.Type == EventPurchase && e.CreatedAt.After(c) {
			boughtAfter[e.UserID] = true
		}
	}
	matched := map[int64]bool{}
	for _, u := range users {
		c, ok := lastCart[u.ID]
		if ok && u.City == city && !boughtAfter[u.ID] && segment.DaysAgo(testNow, c) <= days {
			matched[u.ID] = true
		}
	}
	return matched
}
