package content

import (
	"errors"
	"strings"
	"testing"

	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

// Longest first name in the seed ("Abdurrahmangazi").
var opts = Options{LongestFirstName: 15}

func valid() Content {
	return Content{
		SMS:          "Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! %30 indirim: SEPET30",
		EmailSubject: "{{first_name}}, sepetindekiler %30 indirimli!",
		EmailBody:    "Merhaba {{first_name}},\n\nSepetindeki ürünleri unutmadık.\n\nKeyifli alışverişler!",
	}
}

func errorsOf(t *testing.T, err error) []validation.FieldError {
	t.Helper()
	var ve *validation.Error
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *validation.Error", err)
	}
	return ve.Errors
}

func TestValidContent(t *testing.T) {
	got, err := Validate(valid(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got != valid() {
		t.Fatalf("valid content changed by normalization: %+v", got)
	}
}

func TestParse(t *testing.T) {
	raw := `{"sms":"Selam {{first_name}}!","email_subject":"Fırsat","email_body":"Merhaba"}`
	c, err := Parse([]byte(raw), opts)
	if err != nil || c.SMS != "Selam {{first_name}}!" {
		t.Fatalf("Parse = %+v, %v", c, err)
	}

	for name, raw := range map[string]string{
		"not json":      `{"sms":`,
		"unknown key":   `{"sms":"a","email_subject":"b","email_body":"c","cta":"d"}`,
		"trailing text": `{"sms":"a","email_subject":"b","email_body":"c"} Here you go!`,
		"wrong type":    `{"sms":160,"email_subject":"b","email_body":"c"}`,
	} {
		errs := errorsOf(t, func() error { _, err := Parse([]byte(raw), opts); return err }())
		if errs[0].Code != validation.CodeInvalidJSON {
			t.Errorf("%s: got %+v, want invalid_json", name, errs)
		}
	}

	// A missing key is reported per field, not as a JSON error.
	errs := errorsOf(t, func() error { _, err := Parse([]byte(`{"sms":"a"}`), opts); return err }())
	if len(errs) != 2 || errs[0].Path != "email_subject" || errs[1].Path != "email_body" || errs[0].Code != CodeEmpty {
		t.Fatalf("missing keys: got %+v", errs)
	}
}

func TestNormalization(t *testing.T) {
	c := valid()
	c.SMS = "  Merhaba {{ First_Name }}, {{FIRST_NAME}}!  "
	c.EmailBody = "Satır 1\r\nSatır 2"
	got, err := Validate(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.SMS != "Merhaba {{first_name}}, {{first_name}}!" {
		t.Errorf("sms = %q", got.SMS)
	}
	if got.EmailBody != "Satır 1\nSatır 2" {
		t.Errorf("body = %q", got.EmailBody)
	}
}

// Limits are in runes, and the placeholder is measured as the longest name.
func TestLengthLimits(t *testing.T) {
	ph := len(Placeholder) // 14, ASCII
	cases := []struct {
		name    string
		sms     string
		subject string
		longest int
		tooLong []string // paths expected to be too long
	}{
		{"sms 160 Turkish runes (320 bytes)", strings.Repeat("ş", 160), "a", 15, nil},
		{"sms 161 runes", strings.Repeat("ş", 161), "a", 15, []string{"sms"}},
		{"subject 80 runes", "a", strings.Repeat("ğ", 80), 15, nil},
		{"subject 81 runes", "a", strings.Repeat("ğ", 81), 15, []string{"email_subject"}},
		// 145 + 15-char name = 160: fits exactly.
		{"sms with name at limit", strings.Repeat("ü", 145) + Placeholder, "a", 15, nil},
		{"sms with name one over", strings.Repeat("ü", 146) + Placeholder, "a", 15, []string{"sms"}},
		// Two placeholders count twice: 130 + 2×15 = 160 fits, 131 doesn't.
		{"two names at limit", strings.Repeat("ü", 130) + Placeholder + Placeholder, "a", 15, nil},
		{"two names one over", strings.Repeat("ü", 131) + Placeholder + Placeholder, "a", 15, []string{"sms"}},
		// What matters is the rendered length, not the template's: 164 chars as written,
		// 160 rendered when the longest name is 10 chars...
		{"template over limit, names shorter", strings.Repeat("a", 150) + Placeholder, "a", 10, nil},
		// ...and 160 as written, 161 rendered with a 15-char name.
		{"template fits, names longer", strings.Repeat("a", 160-ph) + Placeholder, "a", 15, []string{"sms"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(Content{SMS: c.sms, EmailSubject: c.subject, EmailBody: "b"}, Options{LongestFirstName: c.longest})
			var got []string
			if err != nil {
				for _, fe := range errorsOf(t, err) {
					if fe.Code != CodeTooLong {
						t.Fatalf("unexpected error %+v", fe)
					}
					got = append(got, fe.Path)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.tooLong, ",") {
				t.Fatalf("too long: %v, want %v (err %v)", got, c.tooLong, err)
			}
		})
	}
}

// The message must tell the model exactly how much to cut.
func TestTooLongMessage(t *testing.T) {
	c := valid()
	c.SMS = strings.Repeat("a", 150) + Placeholder // 150 + 15 = 165
	errs := errorsOf(t, func() error { _, err := Validate(c, opts); return err }())
	msg := errs[0].Message
	for _, want := range []string{"165 characters", "(15 characters)", "limit 160", "shorten by at least 5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Content)
		path string
		code string
	}{
		{"empty sms", func(c *Content) { c.SMS = "" }, "sms", CodeEmpty},
		{"whitespace-only subject", func(c *Content) { c.EmailSubject = "  \n " }, "email_subject", CodeEmpty},
		{"{{name}}", func(c *Content) { c.SMS = "Merhaba {{name}}" }, "sms", CodeUnknownPlaceholder},
		{"{{firstName}}", func(c *Content) { c.SMS = "Merhaba {{firstName}}" }, "sms", CodeUnknownPlaceholder},
		{"{{last_name}}", func(c *Content) { c.EmailBody = "Sayın {{last_name}}" }, "email_body", CodeUnknownPlaceholder},
		{"single braces", func(c *Content) { c.SMS = "Merhaba {first_name}" }, "sms", CodeMalformedPlaceholder},
		{"unclosed", func(c *Content) { c.SMS = "Merhaba {{first_name}" }, "sms", CodeMalformedPlaceholder},
		{"triple braces", func(c *Content) { c.SMS = "Merhaba {{{first_name}}}" }, "sms", CodeMalformedPlaceholder},
		{"percent syntax", func(c *Content) { c.SMS = "Merhaba %first_name%" }, "sms", CodeMalformedPlaceholder},
		{"bracket syntax", func(c *Content) { c.EmailSubject = "[FirstName] için" }, "email_subject", CodeMalformedPlaceholder},
		{"stray brace", func(c *Content) { c.SMS = "Kod: {SEPET30}" }, "sms", CodeMalformedPlaceholder},
		{"newline in subject", func(c *Content) { c.EmailSubject = "Merhaba\nBcc: x@y.z" }, "email_subject", CodeInvalidChar},
		{"control char", func(c *Content) { c.SMS = "Merhaba\x00" }, "sms", CodeInvalidChar},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := valid()
			c.edit(&in)
			errs := errorsOf(t, func() error { _, err := Validate(in, opts); return err }())
			if len(errs) != 1 || errs[0].Path != c.path || errs[0].Code != c.code {
				t.Fatalf("got %+v, want one %s at %s", errs, c.code, c.path)
			}
		})
	}
}

func TestReportsAllErrors(t *testing.T) {
	_, err := Validate(Content{
		SMS:          strings.Repeat("a", 200) + "{{name}}",
		EmailSubject: "",
		EmailBody:    "{first_name}",
	}, opts)
	errs := errorsOf(t, err)
	got := map[string]bool{}
	for _, fe := range errs {
		got[fe.Path+":"+fe.Code] = true
	}
	for _, want := range []string{"sms:" + CodeUnknownPlaceholder, "sms:" + CodeTooLong,
		"email_subject:" + CodeEmpty, "email_body:" + CodeMalformedPlaceholder} {
		if !got[want] {
			t.Errorf("missing %s in %+v", want, errs)
		}
	}
}
