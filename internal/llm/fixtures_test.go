package llm_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
)

// Fixtures must pass the same validation as Claude's output, otherwise the mock demo would
// hit the repair path (or fail) on its happy path.
func TestFixturesAreValid(t *testing.T) {
	v := segment.NewValidator([]string{"İstanbul", "Ankara", "İzmir"})
	placeholder := regexp.MustCompile(`\{\{[^}]*\}\}|\{[^}]*\}`)
	// Longest seeded first name; content must fit even with it rendered.
	const longestName = "Abdurrahmangazi"

	for i, f := range llm.Fixtures() {
		t.Run(fmt.Sprintf("fixture%d", i), func(t *testing.T) {
			if _, err := v.Parse([]byte(f.Audience)); err != nil {
				t.Errorf("audience: %v", err)
			}

			var msg struct {
				SMS          string `json:"sms"`
				EmailSubject string `json:"email_subject"`
				EmailBody    string `json:"email_body"`
			}
			dec := json.NewDecoder(strings.NewReader(f.Messages))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&msg); err != nil {
				t.Fatalf("messages: %v", err)
			}
			if msg.SMS == "" || msg.EmailSubject == "" || msg.EmailBody == "" {
				t.Fatalf("messages: empty field in %+v", msg)
			}
			for field, s := range map[string]string{"sms": msg.SMS, "email_subject": msg.EmailSubject, "email_body": msg.EmailBody} {
				for _, p := range placeholder.FindAllString(s, -1) {
					if p != "{{first_name}}" {
						t.Errorf("%s: unknown placeholder %s", field, p)
					}
				}
			}
			render := func(s string) int {
				return utf8.RuneCountInString(strings.ReplaceAll(s, "{{first_name}}", longestName))
			}
			if n := render(msg.SMS); n > 160 {
				t.Errorf("sms is %d chars rendered, limit 160", n)
			}
			if n := render(msg.EmailSubject); n > 80 {
				t.Errorf("email_subject is %d chars rendered, limit 80", n)
			}
		})
	}
}
