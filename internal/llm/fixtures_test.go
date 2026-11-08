package llm_test

import (
	"fmt"
	"testing"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
)

// Fixtures must pass the same validation as Claude's output, otherwise the mock demo would
// hit the repair path (or fail) on its happy path.
func TestFixturesAreValid(t *testing.T) {
	v := segment.NewValidator([]string{"İstanbul", "Ankara", "İzmir"})
	// Longest seeded first name ("Abdurrahmangazi"); content must fit even with it rendered.
	const longestName = 15

	for i, f := range llm.Fixtures() {
		t.Run(fmt.Sprintf("fixture%d", i), func(t *testing.T) {
			if _, err := v.Parse([]byte(f.Audience)); err != nil {
				t.Errorf("audience: %v", err)
			}
			if _, err := content.Parse([]byte(f.Messages), content.Options{LongestFirstName: longestName}); err != nil {
				t.Errorf("messages: %v", err)
			}
		})
	}
}
