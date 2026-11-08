package content

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderWithName(t *testing.T) {
	got := Render("Merhaba {{first_name}}, {{first_name}} için %30!", "  Şükrü ", "tr")
	if got != "Merhaba Şükrü, Şükrü için %30!" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderWithoutName(t *testing.T) {
	cases := []struct {
		tmpl, lang, want string
	}{
		{"Merhaba {{first_name}}, sepetin seni bekliyor!", "tr", "Merhaba, sepetin seni bekliyor!"},
		{"{{first_name}}, sepetindekiler %30 indirimli!", "tr", "Sepetindekiler %30 indirimli!"},
		{"Sevgili {{first_name}}!", "tr", "Sevgili!"},
		{"Bu fırsat {{first_name}} için hazırlandı.", "tr", "Bu fırsat için hazırlandı."},
		{"{{first_name}}! Sepetin bekliyor", "tr", "Sepetin bekliyor"},
		// Turkish capitalization: i → İ, ı → I.
		{"{{first_name}}, indirim seni bekliyor", "tr", "İndirim seni bekliyor"},
		{"{{first_name}}, ışıl ışıl bir yaz", "tr", "Işıl ışıl bir yaz"},
		{"{{first_name}}, it's waiting", "en", "It's waiting"},
		// Only lines with the placeholder are touched; blank lines and spacing elsewhere stay.
		{"Merhaba {{first_name}},\n\nSepetin  seni bekliyor.\n\nSevgiler", "tr", "Merhaba,\n\nSepetin  seni bekliyor.\n\nSevgiler"},
		{"No placeholder here", "tr", "No placeholder here"},
	}
	for _, c := range cases {
		if got := Render(c.tmpl, "", c.lang); got != c.want {
			t.Errorf("Render(%q, \"\") = %q, want %q", c.tmpl, got, c.want)
		}
		if got := Render(c.tmpl, "   ", c.lang); got != c.want {
			t.Errorf("a whitespace-only name must count as empty: %q", got)
		}
	}
}

func TestRenderFor(t *testing.T) {
	c := Content{
		SMS:          "Merhaba {{first_name}}, sepetin seni bekliyor!",
		EmailSubject: "{{first_name}}, sepetin %30 indirimli",
		EmailBody:    "Merhaba {{first_name}},\n\nSepetin seni bekliyor.",
	}

	r := RenderFor(c, "Ayşe", "tr")
	if r.SMS != "Merhaba Ayşe, sepetin seni bekliyor!" || r.EmailSubject != "Ayşe, sepetin %30 indirimli" ||
		r.EmailBody != "Merhaba Ayşe,\n\nSepetin seni bekliyor." || r.SMSWithoutName || r.EmailWithoutName {
		t.Errorf("with name: %+v", r)
	}

	r = RenderFor(c, "", "tr")
	if r.SMS != "Merhaba, sepetin seni bekliyor!" || r.EmailSubject != "Sepetin %30 indirimli" ||
		r.EmailBody != "Merhaba,\n\nSepetin seni bekliyor." || !r.SMSWithoutName || !r.EmailWithoutName {
		t.Errorf("empty name: %+v", r)
	}

	// No placeholder: nobody is "without name".
	plain := Content{SMS: "Sepetin seni bekliyor!", EmailSubject: "İndirim", EmailBody: "Merhaba,"}
	if r := RenderFor(plain, "", "tr"); r.SMSWithoutName || r.EmailWithoutName {
		t.Errorf("no placeholder: %+v", r)
	}
}

// Safety net: a name longer than the one validation measured with pushes a field over its
// limit; that field falls back to the version without the name, the body keeps it.
func TestRenderForOverLimit(t *testing.T) {
	long := strings.Repeat("A", 40)
	c := Content{
		SMS:          "Merhaba {{first_name}}, " + strings.Repeat("ş", 120),
		EmailSubject: "{{first_name}}, " + strings.Repeat("ğ", 50),
		EmailBody:    "Merhaba {{first_name}},\n\nGövde.",
	}
	r := RenderFor(c, long, "tr")
	if !r.SMSWithoutName || strings.Contains(r.SMS, long) || utf8.RuneCountInString(r.SMS) > SMSMaxRunes {
		t.Errorf("sms: %d runes, without name = %v", utf8.RuneCountInString(r.SMS), r.SMSWithoutName)
	}
	if !r.EmailWithoutName || strings.Contains(r.EmailSubject, long) || utf8.RuneCountInString(r.EmailSubject) > SubjectMaxRunes {
		t.Errorf("subject: %q", r.EmailSubject)
	}
	if !strings.Contains(r.EmailBody, long) {
		t.Errorf("the body has no limit and must keep the name: %q", r.EmailBody)
	}

	// A short name fits: no fallback.
	if r := RenderFor(c, "Ali", "tr"); r.SMSWithoutName || r.EmailWithoutName {
		t.Errorf("short name fell back: %+v", r)
	}
}

// Every valid template, rendered for any name up to the validated length, fits its limits
// and contains no braces.
func TestValidContentAlwaysRendersWithinLimits(t *testing.T) {
	c, err := Validate(Content{
		// "Merhaba " + name + ", " + 134×ü + "!" = 8 + 15 + 2 + 134 + 1 = 160 exactly.
		SMS:          "Merhaba {{first_name}}, " + strings.Repeat("ü", 134) + "!",
		EmailSubject: "{{first_name}}, " + strings.Repeat("ç", 63), // 15 + 2 + 63 = 80
		EmailBody:    "Merhaba {{first_name}},\n\nGövde.",
	}, Options{LongestFirstName: 15})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "A", "Abdurrahmangazi", "İsmail Hakkı"} {
		r := RenderFor(c, name, "tr")
		if n := utf8.RuneCountInString(r.SMS); n > SMSMaxRunes {
			t.Errorf("%q: sms %d runes", name, n)
		}
		if n := utf8.RuneCountInString(r.EmailSubject); n > SubjectMaxRunes {
			t.Errorf("%q: subject %d runes", name, n)
		}
		for _, s := range []string{r.SMS, r.EmailSubject, r.EmailBody} {
			if strings.ContainsAny(s, "{}") {
				t.Errorf("%q: unrendered braces in %q", name, s)
			}
		}
		if name == "Abdurrahmangazi" && (r.SMSWithoutName || r.EmailWithoutName) {
			t.Errorf("the longest validated name must fit without fallback")
		}
	}
}
