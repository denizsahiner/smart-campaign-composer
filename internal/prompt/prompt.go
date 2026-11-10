// Package prompt builds the model requests for each step (system prompt, user turn, output
// schema), parses the answers through the validators, and builds repair requests.
//
// It owns no I/O: the orchestrator decides when to call, repair and retry.
package prompt

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

// Version is stored with every llm_calls row. Bump it whenever a system prompt, a user
// template or a schema changes, so logged results can be tied to the prompt that
// produced them.
const Version = "2026-11-10.1"

// briefBlock wraps the marketer's brief in tags the system prompt tells the model to
// treat as data.
func briefBlock(brief string) string { return block("brief", brief) }

// block wraps text in <tag> … </tag>. A closing tag inside the text is neutralized so the
// text can't end the block early and smuggle in something that reads as instructions.
func block(tag, text string) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "</"+tag+">", "</ "+tag+">")
	return "<" + tag + ">\n" + text + "\n</" + tag + ">"
}

// jsonText marshals v for quoting inside a prompt. Unlike json.Marshal it keeps &, < and >
// as they are, so the model reads "%30 & ücretsiz kargo", not "\u0026".
func jsonText(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Repair returns the request for a repair round: the original request with the previous
// answer and its validation errors appended to the last user turn.
//
// Every round starts from the original request rather than growing a conversation, so the
// prompt stays the same size, and no assistant turn is replayed (current models bind
// thinking blocks to the turn that produced them; replaying an edited turn isn't
// allowed). The model sees what it wrote and exactly what to fix.
func Repair(original llm.Request, previous []byte, verr *validation.Error) llm.Request {
	req := original
	req.Messages = append([]llm.Message(nil), original.Messages...)
	last := len(req.Messages) - 1
	if last < 0 || req.Messages[last].Role != llm.RoleUser {
		panic("prompt.Repair: request must end with a user turn")
	}

	var b strings.Builder
	b.WriteString(req.Messages[last].Content)
	b.WriteString("\n\nYour previous answer was:\n<previous_answer>\n")
	b.Write(previous)
	b.WriteString("\n</previous_answer>\n\nIt was rejected by validation:\n")
	for _, fe := range verr.Errors {
		fmt.Fprintf(&b, "- %s: %s\n", fe.Path, fe.Message)
	}
	b.WriteString("\nReturn a complete, corrected answer. Fix every problem listed and keep everything else as it was.")
	for _, fe := range verr.Errors {
		if fe.Code == content.CodeTooLong {
			// Character counts are hard for models; asking to cut only the minimum
			// usually lands just over the limit again.
			b.WriteString(" When shortening, cut clearly more than the minimum: aim at least 15 characters below the limit.")
			break
		}
	}
	req.Messages[last].Content = b.String()
	return req
}
