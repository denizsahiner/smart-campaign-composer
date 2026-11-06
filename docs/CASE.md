# Smart Campaign Composer — Case Requirements

> Source: Insider One, AI Software Engineer case study (2026), translated from Turkish. Deadline: 7 days from receipt (November 11, 2026).

An end-to-end system that uses Claude to turn a marketer's natural-language campaign request into a target audience and SMS / email messages, presents them for approval, sends them, and shows delivery status live.

- **Frontend:** Vue.js
- **Backend:** Go or PHP
- **LLM:** Claude API

## Business context

Insider One customers send SMS and email to millions of people every day. Preparing a campaign means defining the audience, writing copy for each channel, and staying within character limits. The marketer only writes what they want; Claude proposes the rest; a human makes the final decision.

## User flow

Example brief:

> "Send a 30% discount reminder to users in Istanbul who added items to their cart in the last 30 days but did not make a purchase. Keep it friendly but short."

1. **Audience is extracted:** Claude produces a segment rule from the brief; the UI shows how many people it will reach.
2. **Messages are generated:** SMS and email content streams onto the screen.
3. **Human approves:** The marketer previews, edits if needed, and approves.
4. **Messages are sent:** Messages go through a queue to a mock provider.
5. **Live monitoring:** A dashboard shows delivery status.

No real person receives a message. Recipients are fake users, and sending goes to a mock provider.

## Functional requirements

### Audience
- The segment rule produced by Claude is validated in the backend; LLM output is never executed directly as SQL.
- Users without consent for a channel are not messaged on that channel.

### Content
- Channel limits: SMS at most 160 characters, email subject at most 80 characters.
- Personalization variables such as `{{first_name}}` are supported.

### Sending
- Asynchronous queue, at most 100 messages per second per channel.
- The same message is never sent twice to the same person. The retry strategy is up to the candidate.

### Monitoring
- Live delivery status, metrics, and health check endpoints.

## AI requirements (most important part)

Calling Claude is not enough; its output must be made reliable.

- **Validation:** Claude's output must be structured (JSON) and checked in the backend.
- **Testability:** Tests must run without a real API key, using a mock LLM.

## Resources

- **Claude API key:** read from `ANTHROPIC_API_KEY` in `.env`. Add a `.env.example` to the repo. Development can use the mock LLM.
- **Fake user data:** a seed of a few thousand users. Fields (name, email, phone, city, purchase history, channel consents, etc.) are up to the candidate.
- **Mock SMS/email provider:** a simple service imitating a real one. It should occasionally return errors, delays, and 429s so that retry and rate-limit logic can actually be tested.

## Technology

Frontend Vue.js, backend Go or PHP. Database, queue, and libraries are the candidate's choice.

## Deliverables

- GitHub or GitLab repository
- One-command setup with `docker compose up` (including seed data and mock provider)
- Tests that run with `make test`
- `README.md`: setup and a short architecture overview
- `DECISIONS.md`: key decisions and their rationale
- `ai-sessions/`: Claude session exports

**Scope note:** A solid core flow is preferred over doing everything. List anything not completed in the README.

## Bonus (optional)

- Scheduled sending
- A/B variants
- Content in the user's language
- CI pipeline

## Use of AI tools

Tools such as Claude Code and Cursor are allowed, even expected. What matters is understanding and owning the generated code.

Add exports of all Claude working sessions during the case to the `ai-sessions/` folder (`/export` in Claude Code, a share link on claude.ai). Remove API keys and personal information before submitting.

Sessions are not scored by how much AI was used. They are used to see how the problem was approached and how AI output was verified.

## Evaluation

| Criterion | Weight |
|---|---|
| LLM integration | 25% |
| Architecture and decisions | 20% |
| Backend reliability | 20% |
| Frontend | 20% |
| Testing and code quality | 15% |

## Next step

Results are communicated within one week of submission. If positive, a 30-minute review meeting follows to discuss the solution and the decisions made.