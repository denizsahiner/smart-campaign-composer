package llm

// Fixture is a canned audience + messages reply for one brief.
type Fixture struct {
	Brief    string
	Audience string
	Messages string
}

// ExampleBriefTR and ExampleBriefEN are the case's example brief (original Turkish and
// its translation). Both map to the same segment; content follows the brief's language.
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
		Messages: `{"sms":"Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! Bugüne özel %30 indirimle siparişini tamamla.",` +
			`"email_subject":"{{first_name}}, sepetindekiler %30 indirimli!",` +
			`"email_body":"Merhaba {{first_name}},\n\nSepetine eklediğin ürünleri unutmadık. Siparişini bugün tamamlarsan %30 indirim senin.\n\nKeyifli alışverişler!"}`,
	},
	{
		Brief:    ExampleBriefEN,
		Audience: exampleAudience,
		Messages: `{"sms":"Hi {{first_name}}, your cart is waiting! Complete your order today and get 30% off.",` +
			`"email_subject":"{{first_name}}, your cart is 30% off today",` +
			`"email_body":"Hi {{first_name}},\n\nYou left a few things in your cart. Finish your order today and get 30% off.\n\nHappy shopping!"}`,
	},
}

// defaultFixture answers any brief without a dedicated fixture, so the mock never blocks
// the demo flow. It's a fixed, generic reply: the mock does not interpret briefs.
var defaultFixture = Fixture{
	Audience: `{"rules":[{"field":"cart_added_days_ago","op":"lte","value":30}],"unsupported_criteria":[]}`,
	Messages: `{"sms":"Merhaba {{first_name}}, sana özel fırsatlar seni bekliyor! Hemen göz at.",` +
		`"email_subject":"{{first_name}}, sana özel fırsatlar",` +
		`"email_body":"Merhaba {{first_name}},\n\nSana özel fırsatları kaçırma.\n\nKeyifli alışverişler!"}`,
}

// Fixtures returns all fixtures including the default one (Brief == ""), for tests that
// check every canned reply passes validation.
func Fixtures() []Fixture { return append(append([]Fixture(nil), fixtures...), defaultFixture) }
