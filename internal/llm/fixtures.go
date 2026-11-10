package llm

// Fixture is a canned audience + messages reply for one brief. Messages has one content
// object per language; the mock answers with the languages the request's schema asks for.
type Fixture struct {
	Brief    string
	Audience string
	Messages map[string]string
}

// ExampleBriefTR and ExampleBriefEN are the case's example brief (original Turkish and
// its translation). Both map to the same segment and get messages in both languages: the
// language of the messages follows the recipients, not the brief.
const (
	ExampleBriefTR = "Son 30 gün içinde sepetine ürün ekleyip satın alma yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun."
	ExampleBriefEN = "Send a 30% discount reminder to users in Istanbul who added items to their cart in the last 30 days but did not make a purchase. Keep it friendly but short."
)

const exampleAudience = `{"rules":[` +
	`{"field":"city","op":"eq","value":"Istanbul"},` +
	`{"field":"cart_added_days_ago","op":"lte","value":30},` +
	`{"field":"purchased_after_cart","op":"eq","value":false}],` +
	`"unsupported_criteria":[]}`

// fixtures are matched in order; the first whose brief appears in the request wins.
// Content follows the prompt's rules too: no discount codes or links the brief didn't give.
var fixtures = []Fixture{
	{
		Brief:    ExampleBriefTR,
		Audience: exampleAudience,
		Messages: map[string]string{
			"tr": `{"sms":"Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! Bugüne özel %30 indirimle siparişini tamamla.",` +
				`"email_subject":"{{first_name}}, sepetindekiler %30 indirimli!",` +
				`"email_body":"Merhaba {{first_name}},\n\nSepetine eklediğin ürünleri unutmadık. Siparişini bugün tamamlarsan %30 indirim senin.\n\nKeyifli alışverişler!"}`,
			"en": `{"sms":"Hi {{first_name}}, the items in your cart are waiting! Complete your order today with 30% off.",` +
				`"email_subject":"{{first_name}}, your cart is 30% off!",` +
				`"email_body":"Hi {{first_name}},\n\nWe saved the items you added to your cart. Complete your order today and get 30% off.\n\nHappy shopping!"}`,
		},
	},
	{
		Brief:    ExampleBriefEN,
		Audience: exampleAudience,
		Messages: map[string]string{
			"tr": `{"sms":"Merhaba {{first_name}}, sepetin seni bekliyor! Bugün tamamlarsan %30 indirim senin.",` +
				`"email_subject":"{{first_name}}, sepetin bugün %30 indirimli",` +
				`"email_body":"Merhaba {{first_name}},\n\nSepetinde birkaç ürün unuttun. Siparişini bugün tamamla, %30 indirim kazan.\n\nKeyifli alışverişler!"}`,
			"en": `{"sms":"Hi {{first_name}}, your cart misses you! Check out today and get 30% off.",` +
				`"email_subject":"{{first_name}}, 30% off your cart today",` +
				`"email_body":"Hi {{first_name}},\n\nYou left a few items in your cart. Check out today and save 30%.\n\nHappy shopping!"}`,
		},
	},
}

// defaultFixture answers any brief without a dedicated fixture, so the mock never blocks
// the demo flow. It's a fixed, generic reply: the mock does not interpret briefs.
var defaultFixture = Fixture{
	Audience: `{"rules":[{"field":"cart_added_days_ago","op":"lte","value":30}],"unsupported_criteria":[]}`,
	Messages: map[string]string{
		"tr": `{"sms":"Merhaba {{first_name}}, sana özel fırsatlar seni bekliyor! Hemen göz at.",` +
			`"email_subject":"{{first_name}}, sana özel fırsatlar",` +
			`"email_body":"Merhaba {{first_name}},\n\nSana özel fırsatları kaçırma.\n\nKeyifli alışverişler!"}`,
		"en": `{"sms":"Hi {{first_name}}, special offers are waiting for you! Take a look.",` +
			`"email_subject":"{{first_name}}, offers picked for you",` +
			`"email_body":"Hi {{first_name}},\n\nDon't miss the offers we picked for you.\n\nHappy shopping!"}`,
	},
}

// Fixtures returns all fixtures including the default one (Brief == ""), for tests that
// check every canned reply passes validation.
func Fixtures() []Fixture { return append(append([]Fixture(nil), fixtures...), defaultFixture) }
