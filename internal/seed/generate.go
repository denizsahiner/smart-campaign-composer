// Package seed generates and loads the fake user base: users plus their cart/purchase events.
package seed

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

const day = 24 * time.Hour

const (
	EventCartAdd  = "cart_add"
	EventPurchase = "purchase"
)

// Options controls the size and shape of the generated data set.
type Options struct {
	// Users is the total number of users, including the fixed edge-case users
	// (which are always generated, even if Users is smaller).
	Users int
	// Seed makes generation deterministic: the same seed yields the same data.
	Seed uint64
	// Now is the reference time. Event times are relative to it, so "last 30 days"
	// stays meaningful whenever the seed is loaded.
	Now time.Time
}

type User struct {
	ID           int64
	FirstName    string
	LastName     string
	Email        *string
	Phone        *string
	City         string
	Language     string
	SMSConsent   bool
	EmailConsent bool
	CreatedAt    time.Time
}

type Event struct {
	UserID    int64
	Type      string
	CreatedAt time.Time
}

// Generate returns users and events. It is a pure function: no DB, no clock, no global rand.
func Generate(opts Options) ([]User, []Event) {
	g := &generator{
		rng: rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15)),
		now: opts.Now,
	}
	g.addEdgeCases()
	for len(g.users) < opts.Users {
		g.addRandomUser()
	}
	return g.users, g.events
}

type generator struct {
	rng    *rand.Rand
	now    time.Time
	users  []User
	events []Event
}

// newUser builds the next user (not yet added) with fully reachable defaults.
func (g *generator) newUser(first, last, city string) User {
	id := int64(len(g.users) + 1)
	return User{
		ID:           id,
		FirstName:    first,
		LastName:     last,
		Email:        ptr(email(first, last, id)),
		Phone:        ptr(g.phone()),
		City:         city,
		Language:     "tr",
		SMSConsent:   true,
		EmailConsent: true,
		// Older than any generated event (at most ~300 days back).
		CreatedAt: g.ago(365 + g.rng.IntN(365)),
	}
}

func (g *generator) add(u User) { g.users = append(g.users, u) }

func (g *generator) event(userID int64, typ string, at time.Time) {
	g.events = append(g.events, Event{UserID: userID, Type: typ, CreatedAt: at})
}

// ago returns a time exactly `days` whole days back, at a random point within that day,
// so floor((now - t) / day) == days.
func (g *generator) ago(days int) time.Time {
	return g.now.Add(-time.Duration(days)*day - time.Duration(g.rng.IntN(86400))*time.Second)
}

func (g *generator) phone() string {
	return fmt.Sprintf("+90%s%07d", pick(g.rng, mobilePrefixes), g.rng.IntN(10_000_000))
}

func (g *generator) addRandomUser() {
	u := g.newUser(pick(g.rng, firstNames), pick(g.rng, lastNames), weightedPick(g.rng, cities))
	if g.rng.Float64() < 0.15 {
		u.Language = "en"
	}
	if g.rng.Float64() < 0.08 {
		u.Email = nil
	}
	if g.rng.Float64() < 0.10 {
		u.Phone = nil
	}
	u.SMSConsent = g.rng.Float64() < 0.70
	u.EmailConsent = g.rng.Float64() < 0.80
	g.add(u)
	g.addBehavior(u.ID)
}

// addBehavior assigns a behavior profile and the events that go with it.
func (g *generator) addBehavior(userID int64) {
	switch r := g.rng.Float64(); {
	case r < 0.40: // passive: no events
	case r < 0.65: // cart abandoner: recent cart adds, nothing bought since
		if g.rng.Float64() < 0.3 {
			g.event(userID, EventPurchase, g.ago(60+g.rng.IntN(240)))
		}
		for range 1 + g.rng.IntN(3) {
			g.event(userID, EventCartAdd, g.ago(g.rng.IntN(30)))
		}
	case r < 0.85: // buyer: the cart add is followed by a purchase within two days
		cart := g.ago(3 + g.rng.IntN(178))
		g.event(userID, EventCartAdd, cart)
		g.event(userID, EventPurchase, cart.Add(time.Duration(1+g.rng.IntN(48))*time.Hour))
	case r < 0.95: // stale cart: last cart add is older than 30 days, never bought
		g.event(userID, EventCartAdd, g.ago(31+g.rng.IntN(150)))
	default: // returning: bought before, then added to cart again recently
		g.event(userID, EventPurchase, g.ago(15+g.rng.IntN(46)))
		g.event(userID, EventCartAdd, g.ago(g.rng.IntN(14)))
	}
}

// addEdgeCases adds fixed users that random generation might not produce, so the
// rules decided in the architecture are always testable against the seed.
// All are in İstanbul so they meet the example brief's city criterion.
func (g *generator) addEdgeCases() {
	const city = "İstanbul"

	// Very long first name: pushes a rendered SMS over the limit (over-limit flow).
	u := g.newUser("Abdurrahmangazi", "Karamustafaoğlu", city)
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(5))

	// Empty first name: exercises the "Merhaba," fallback.
	u = g.newUser("", "Yılmaz", city)
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(3))

	// Last cart add 29 days ago: just inside a 30-day window.
	u = g.newUser("Selin", "Aydın", city)
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(29))

	// Last cart add 31 days ago: just outside a 30-day window.
	u = g.newUser("Kerem", "Aydın", city)
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(31))

	// Bought two hours after adding to cart: not an abandoned cart.
	u = g.newUser("Deniz", "Kaya", city)
	g.add(u)
	cart := g.ago(10)
	g.event(u.ID, EventCartAdd, cart)
	g.event(u.ID, EventPurchase, cart.Add(2*time.Hour))

	// Bought, then added to cart again: abandoned (the purchase is before the last cart add).
	u = g.newUser("Ozan", "Demir", city)
	g.add(u)
	g.event(u.ID, EventPurchase, g.ago(20))
	g.event(u.ID, EventCartAdd, g.ago(5))

	// SMS consent but no phone: dropped by the mandatory SMS filter.
	u = g.newUser("Ceren", "Polat", city)
	u.Phone = nil
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(7))

	// Email consent but no email: dropped by the mandatory email filter.
	u = g.newUser("Berk", "Koç", city)
	u.Email = nil
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(7))

	// No consent on any channel: in the segment but reachable on neither channel.
	u = g.newUser("Nazlı", "Şen", city)
	u.SMSConsent, u.EmailConsent = false, false
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(7))

	// Recipients the mock provider rejects with a permanent 400, so the "failed without
	// retry" path shows up in every demo. A landline is not a Turkish mobile number.
	u = g.newUser("Emre", "Çetin", city)
	u.Phone = ptr("+902125550123")
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(6))

	// .invalid is reserved (RFC 2606) and can never be a real domain.
	u = g.newUser("Gizem", "Arslan", city)
	u.Email = ptr("gizem.arslan@bounce.invalid")
	g.add(u)
	g.event(u.ID, EventCartAdd, g.ago(6))
}

// email builds a unique, ASCII address on example.com (RFC 2606: never a real mailbox).
func email(first, last string, id int64) string {
	var parts []string
	for _, p := range []string{first, last} {
		if p != "" {
			parts = append(parts, asciiLower(p))
		}
	}
	return fmt.Sprintf("%s.%d@example.com", strings.Join(parts, "."), id)
}

var asciiReplacer = strings.NewReplacer(
	"ç", "c", "Ç", "c", "ğ", "g", "Ğ", "g", "ı", "i", "İ", "i",
	"ö", "o", "Ö", "o", "ş", "s", "Ş", "s", "ü", "u", "Ü", "u",
)

func asciiLower(s string) string { return strings.ToLower(asciiReplacer.Replace(s)) }

type weighted struct {
	value  string
	weight int
}

func weightedPick(rng *rand.Rand, xs []weighted) string {
	total := 0
	for _, x := range xs {
		total += x.weight
	}
	n := rng.IntN(total)
	for _, x := range xs {
		if n < x.weight {
			return x.value
		}
		n -= x.weight
	}
	panic("unreachable")
}

func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.IntN(len(xs))] }

func ptr[T any](v T) *T { return &v }
