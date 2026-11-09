# Architecture Draft

## Stack
- Backend language: Go
- Database: PostgreSQL
- Queue: Postgres table 
- Real-time updates to the frontend: SSE

## Services
- Containers run in docker compose: postgres, api, worker, mock-provider, frontend
- One Go binary, three long-running entrypoints (`./app api`, `./app worker`,
  `./app mock-provider`), each its own container. Shares the same models, LLMClient, and
  validation code. Kept as one binary to avoid duplicating that code; run as separate
  containers so a crash in one doesn't take down the others.
- A third entrypoint, `./app migrate`, runs as a one-shot `migrate` service: applies the
  schema and loads a deterministic seed (fixed random seed, so tests and demos see the same
  data; the example brief is guaranteed to match more than 0 users).
- Startup order: `postgres` (healthcheck: `pg_isready`) → `migrate`
  (`depends_on: postgres: service_healthy`) → `api` and `worker`
  (`depends_on: migrate: service_completed_successfully`). Kept separate from API startup so
  API restarts don't re-run migrations and responsibilities stay clear. `worker` also
  waits for `mock-provider: service_healthy`.
- `frontend`: the Vue app is built and served by nginx, which proxies `/api` to the API
  (no CORS setup needed).
- Graceful shutdown: on SIGTERM the worker stops claiming, lets sends already on the wire
  finish (bounded by the HTTP timeout) and writes their results, hands claimed rows still
  waiting for the rate limiter back with their attempt refunded, and exits. A row whose
  result couldn't be written stays `sending` and is recovered by lease expiry. The API drains
  open requests and closes SSE streams.

## Configuration
Environment variables, all with working defaults (`.env.example` lists them):

| Variable | Default | Used by |
|---|---|---|
| `DATABASE_URL` | set in compose | migrate, api, worker |
| `API_ADDR` | `:8080` | api |
| `ANTHROPIC_API_KEY`, `LLM_PROVIDER`, `ANTHROPIC_MODEL`, `LLM_EFFORT_AUDIENCE`, `LLM_EFFORT_MESSAGES`, `LLM_REFUSAL_FALLBACK` | see LLM | api |
| `PROVIDER_URL` | `http://mock-provider:8090` | worker |
| `WORKER_HEALTH_ADDR` | `:8081` | worker |
| `WORKER_RATE_PER_CHANNEL` | `100` | worker |
| `WORKER_CONCURRENCY_PER_CHANNEL` | `32` | worker |
| `WORKER_BATCH_SIZE` | `20` | worker |
| `WORKER_LEASE` | `30s` | worker |
| `WORKER_MAX_ATTEMPTS` | `5` | worker |
| `WORKER_BACKOFF_BASE` | `1s` | worker |
| `WORKER_HTTP_TIMEOUT` | `3s` | worker |
| `WORKER_POLL_INTERVAL` | `200ms` (when the queue is empty) | worker |
| `PROVIDER_ADDR` | `:8090` | mock-provider |
| `PROVIDER_RATE_PER_CHANNEL` | `100` | mock-provider |
| `PROVIDER_ERROR_RATE` | `0.05` | mock-provider |
| `PROVIDER_TIMEOUT_RATE` | `0.01` | mock-provider |
| `PROVIDER_THROTTLE_EVERY`, `PROVIDER_THROTTLE_FOR` | `30s`, `2s` | mock-provider |
| `PROVIDER_LATENCY_MIN_MS`, `PROVIDER_LATENCY_MAX_MS` | `50`, `300` | mock-provider |
| `PROVIDER_SEED` | empty (random) | mock-provider |

Ports published to the host: 8080 (API) and 8090 (mock provider, so `/deliveries` can be
opened). The worker's 8081 stays inside the compose network.

## Data
- User fields: 
  first_name, last_name, email (nullable), phone (nullable), city, language (tr/en),
  sms_consent, email_consent, created_at.
  Segment DSL fields map either to a column on `users` (`city`, `language`) or to a derived
  expression the query builder computes from `user_events` (e.g. `cart_added_days_ago`).
  Contact fields (`first_name`, `last_name`, `email`, `phone`) and consent fields are not
  segment criteria. email/phone nullable to simulate real-world missing data.
- Consent is not part of the DSL. `sms_consent` / `email_consent` are never accepted from
  Claude's output (a rule on them fails validation). Instead the backend always applies a
  hard-coded per-channel filter (`sms_consent AND phone IS NOT NULL` for SMS,
  `email_consent AND email IS NOT NULL` for email) on top of whatever segment was generated.
  Consent is a compliance constraint, not a targeting choice, so it lives in code where the
  LLM can neither relax nor override it.
  Trade-off: briefs like "email users who have no SMS consent" can't be expressed; accepted
  for this scope.
- cart/purchase history:
  Separate `user_events` table: user_id, type ('cart_add' | 'purchase'), created_at.
  Chosen over summary columns on `users` because time-window queries ("last 30 days") and "added but didn't buy" logic map naturally onto it.
- Seed data: generated in Go (`internal/seed`) by `./app migrate` after migrations, only
  if `users` is empty. 5,000 users from a fixed random seed, so every install has the same
  data. Hand-curated Turkish name/city pools only, so Turkish characters are exercised.
  `en` users also have Turkish names (Turkish users who prefer English). Event times are
  relative to load time so "last 30 days" stays meaningful. Each user gets a behavior
  profile (passive / cart abandoner / buyer / stale cart / returning). Fixed edge-case users
  are always included (long name, empty name, 29/31 days, bought after cart, missing
  phone/email, no consent, and two recipients the provider rejects: a phone number that
  isn't a Turkish mobile and an email on a `.invalid` domain). Loaded with `COPY` in one
  transaction.
- LLM call log: `llm_calls` table, one row per Claude call: campaign_id, step
  ('audience' | 'messages'), prompt_version, raw_response, validation_errors (jsonb),
  repair_attempt, transport_attempt, input_tokens, output_tokens, latency_ms, created_at.
  Feeds the LLM numbers in `/metrics`, makes failures debuggable, and gives concrete
  evidence of how Claude's output was validated and repaired.
- `campaigns` table:
  id, brief (text), segment_rule (jsonb), sms_content, email_subject, email_body,
  status (`draft` | `ready` | `approved` | `completed`), sms_audience_count,
  email_audience_count, over_limit_policy (`fallback` | `skip`), created_at, updated_at,
  approved_at.
  - Lifecycle: `draft` (brief entered, LLM steps running / being edited) → `ready` (segment
    and content validated) → `approved` (messages queued, sending in progress) →
    `completed` (no message left in `pending` / `sending` / `retry_scheduled`).
  - Audience counts are per channel because the consent filter is per channel.
  - Content columns are editable until approval and immutable afterwards. Claude's original
    draft is not duplicated here; it's in `llm_calls.raw_response`.
  - `completed` is set by the worker after each batch with a conditional update:
    `UPDATE campaigns SET status = 'completed' WHERE id = $1 AND status = 'approved' AND NOT
    EXISTS (SELECT 1 FROM messages WHERE campaign_id = $1 AND status IN ('pending',
    'sending', 'retry_scheduled'))`.
- `messages` table:
  id, campaign_id, user_id, channel (`sms` | `email`), to_address, subject, body, status,
  attempts, next_attempt_at, locked_until, last_error, provider_message_id, sent_at,
  created_at, updated_at. `UNIQUE (campaign_id, user_id, channel)`.
  - `body` is the SMS text or the email body; `subject` is email-only
    (`CHECK (channel = 'email' OR subject IS NULL)`).
  - `to_address` snapshots the phone/email at enqueue time: the row is self-contained for
    audit and the worker doesn't join `users`.
  - `status` CHECK: `pending` | `sending` | `sent` | `retry_scheduled` | `failed` |
    `skipped` (recipients skipped by `over_limit_policy = skip`, so nobody is dropped
    silently and the dashboard can count them).
  - `next_attempt_at NOT NULL DEFAULT now()`: a NULL would make `next_attempt_at <= now()`
    false and `pending` rows would never be claimed.
  - `provider_message_id` is the ID the provider returns. The idempotency key we send to
    the provider is our own `messages.id`.
- Indexes:
  ```sql
  -- worker claim: partial indexes stay small since they only contain rows still awaiting work
  -- (channel first: each channel's loop claims only its own rows)
  CREATE INDEX ON messages (channel, next_attempt_at) WHERE status IN ('pending', 'retry_scheduled');
  CREATE INDEX ON messages (channel, locked_until)    WHERE status = 'sending';
  -- dashboard aggregate (GROUP BY channel, status), index-only scan
  CREATE INDEX ON messages (campaign_id, channel, status);
  -- segment: "most recent cart_add" and "purchase after it" per user
  CREATE INDEX ON user_events (user_id, type, created_at);
  ```
  At a few thousand rows any query is fast without indexes; these document which query each
  access path serves.

## LLM
Claude is used as a proposal generator, never as a decision-maker — its output always
passes through a gate before it reaches SQL or the user.

- What does Claude return for the audience step? 
  ```json
  {
    "rules": [
      {"field": "city", "op": "eq", "value": "Istanbul"},
      {"field": "cart_added_days_ago", "op": "lte", "value": 30},
      {"field": "purchased_after_cart", "op": "eq", "value": false}
    ],
    "unsupported_criteria": []
  }
  ```
  Only whitelisted fields/operators are allowed. This JSON is never run as SQL directly —
  it's translated by a query builder that maps each field to a parameterized SQL fragment.

  DSL semantics:
  - All rules are combined with AND. OR / NOT are out of scope.
  - Whitelist, with allowed operators and value types per field:

    | Field                  | Ops                    | Value                         | Meaning |
    |------------------------|------------------------|-------------------------------|---------|
    | `city`                 | `eq`, `in`             | string / string[] (known city)| `users.city` |
    | `language`             | `eq`                   | `"tr"` \| `"en"`              | `users.language` |
    | `cart_added_days_ago`  | `lte`, `gte`           | int, 1–365                    | calendar days (Europe/Istanbul) between the user's **most recent** `cart_add` and the reference time: today = 0, yesterday = 1; users with no `cart_add` never match |
    | `purchased_after_cart` | `eq`                   | bool                          | whether a `purchase` event exists strictly **after the most recent** `cart_add`; users with no `cart_add` never match (for either value) |

    So "added to cart in the last 30 days but didn't buy" = `cart_added_days_ago lte 30`
    AND `purchased_after_cart eq false`.
  - Day windows are calendar days, not rolling 24h windows: with a reference time of
    Nov 7 14:00, `lte 30` starts at Oct 8 00:00 Istanbul time (Oct 7 21:00 UTC). The
    boundary instants are computed in Go and passed as `timestamptz` parameters.
  - The reference time (`as_of`) is pinned when the audience is previewed and reused at
    approval, so the window doesn't move between the two (e.g. across midnight). The
    segment itself is still re-evaluated at approval, so consent changes and new events
    are picked up.
  - Any other field, an operator not allowed for that field, a wrong value type, or an
    out-of-range value fails validation.
  - City normalization: the list of cities present in the data is given to Claude in the
    prompt, and the backend normalizes values (case + Turkish diacritics, e.g. "Istanbul" /
    "istanbul" → "İstanbul") before checking them against that list. An unknown city fails
    validation instead of silently producing a 0-person segment.
  - Unsupported criteria: parts of the brief the DSL can't express (age, order value, a
    city not in the data, "opened my last email") go to `unsupported_criteria` instead of
    being approximated with another rule or dropped silently. The UI shows them, and the
    marketer must explicitly acknowledge them before moving on to the messages step (the
    API rejects the step otherwise): "Paris'te sepeti terk edenler" would otherwise become
    every cart abandoner in every city behind a warning nobody read. An answer with no
    rules but a reason there is final (the marketer has to
    change the brief), not a repair case. A 0-person segment blocks the flow until the
    brief is edited.

- What does Claude return for the message step?
  ```json
  {
    "sms": "Merhaba {{first_name}}, sepetin seni bekliyor! %30 indirim...",
    "email_subject": "Sepetindeki ürünler için %30 indirim",
    "email_body": "..."
  }
  ```
  Streamed to the frontend as it's generated (SSE). This is a draft shown to the marketer
  for approval, not the final sent content.

  Validation can only run once the stream is complete, so the SSE stream uses typed events:
  - `event: delta`: partial content, rendered in the UI as an unvalidated draft.
  - `event: validated`: the final, validated JSON. The approve button is only enabled
    after this event.
  - `event: retry`: validation failed and Claude is asked to fix it; the UI clears the
    draft and shows the new stream.
  - `event: invalid`: repair attempts exhausted. The last draft is shown, editable, with
    the failing fields marked (e.g. "SMS is 168 characters, cut 8"); the approve button
    stays disabled until the content passes the same validator the approve endpoint runs.
    The marketer is in the loop anyway, so a draft that is 8 characters too long is
    handed to them instead of ending the flow.
  - `event: error`: no usable answer at all (transport retries exhausted, refusal).

  Guided regeneration: the marketer can ask for a new version with a short note ("more
  formal", "lead with the discount"). The request quotes the previous draft and the note;
  it's an ordinary messages step with the same validation, repairs and events. Manual
  editing stays available.

  The audience step has no editable fallback: if repairs run out, the last validation
  errors are shown in plain words ("Paris is not in the data") and the marketer edits the
  brief. A segment editor UI is out of scope.

- How is structured output obtained from Claude?
  Each step (audience, messages) has its own JSON schema, sent as a structured output
  format (`output_config.format`) instead of "return only JSON" in the prompt. This
  removes almost all parse failures (stray text, broken JSON). The answer is JSON text, so
  for the message step the text deltas feed the SSE stream directly.
  Forced tool use (`tool_choice: tool`) was the original plan; current models (Claude
  Opus 5.5) reject it with a 400, and structured outputs is the replacement for exactly
  this case.
  The audience schema is generated from the segment whitelist, one `anyOf` variant per
  field, so schema and validator can't drift apart. Cities are a free string, not an
  enum: constrained decoding would force an unknown city ("Paris") onto a known one.
  Structured outputs don't support `minimum`/`maximum`/`minLength`/`maxLength`, so the
  1–365 day range and the character limits can only be enforced by the validator. Schema
  conformance is not business-rule conformance, so the backend validation below runs on
  every response.
  Two steps, two schemas: the marketer sees the reach before messages are generated, the
  messages need the segment's longest name, and a too-long SMS is repaired without
  regenerating the segment.

- What is checked before accepting Claude's output?
  - Valid JSON, matches expected schema
  - Segment: every field/operator in the whitelist (consent fields are never whitelisted)
  - SMS ≤ 160 chars, email subject ≤ 80 chars (checked against the longest name in the
    matched segment, since {{first_name}} changes per user).
    Length is counted in Unicode characters (`utf8.RuneCountInString`), not bytes — Go's
    `len()` would count "ş" as 2. GSM-7 vs. UCS-2 encoding (where Turkish characters drop
    the real SMS limit to 70) is deliberately out of scope; the case's 160-character rule is
    applied as-is (documented in DECISIONS.md).
  - Only known placeholders used (e.g. {{first_name}})
  - On failure: send the validation error back to Claude and ask it to fix it (see retries
    below), then stop instead of looping forever (see `event: invalid`).
    A repair round is the original request plus a user turn quoting the previous answer
    and the errors; the previous assistant turn is not replayed, since current models
    bind thinking blocks to the turn that produced them.

- How are Claude calls retried?
  Two separate counters that never consume each other's budget:
  - Repair (validation) retry: the response arrived but breaks a business rule —
    typically SMS > 160 chars (LLMs are bad at counting, especially Turkish text), subject
    > 80, unknown placeholders (`{{name}}`, `{first_name}`), an operator not allowed for a
    field, an out-of-range value, or an unknown city. Fixes that code can do safely (city
    spelling) are normalized before validation; fixes that need meaning preserved
    (shortening copy — truncating would cut "…%30 indi") go back to Claude with a concrete
    error, e.g. "SMS is 178 chars, limit 160: shorten by 18+; `{{first_name}}` can be up to
    12 chars". At most 2 repair attempts per step.
  - Transport retry: the request didn't get a usable response — timeout, 429, 529, 5xx.
    Exponential backoff with equal jitter (between d/2 and d for d = 1s, 2s), at most 2
    retries per step; `retry-after` is respected when longer than the backoff. The
    marketer is waiting on screen, so the budget is short: if `retry-after` exceeds 10s,
    fail immediately with "Claude is busy, try again" instead of waiting. 400/401 and
    refusals are not retried.
  - Both budgets are per step, not per call, so a step makes at most 1 + 2 + 2 = 5 calls
    (per-call transport budgets would allow 3 × 3 = 9). A transport retry resends the
    same request, so a retry in the middle of a repair round resends that repair.
  - Every call, failed ones included, is written to `llm_calls` with its repair and
    transport attempt numbers, even if the marketer's request was cancelled meanwhile.
  - Timeouts: 15s for the stream to start, 20s idle between stream events (a single
    total timeout would kill legitimately long email generations). Both are timeout
    transport errors, retried like any other; a stall after output started is marked
    partial so the UI discards the draft.
  - Streaming: transport retry is silent only before the first token. If the stream breaks
    mid-way, the UI gets `event: retry` and generation restarts (a half-received JSON can't
    be resumed).
  - The Anthropic SDK's built-in retries are disabled (`WithMaxRetries(0)`) so retries
    don't stack and the rules above stay in our control. Verified: the Go SDK (v2.7) retries
    twice by default, which would turn one 429 with `retry-after: 7` into 3 requests and
    14s of hidden waiting; a test asserts one request per call.
  - Errors are classified from the HTTP status, or, for an error event inside a 200
    stream (e.g. `overloaded_error` mid-generation), from the error type. A refusal
    (`stop_reason: refusal`) is neither retried nor repaired; with `fallbacks: "default"`
    the API first re-runs a declined request on another model server-side (not on Haiku,
    which has no fallback). `max_tokens` truncation is an error, not a repair case.
  - Model and per-step effort are configuration (`ANTHROPIC_MODEL`,
    `LLM_EFFORT_AUDIENCE` / `LLM_EFFORT_MESSAGES`; defaults Claude Sonnet 5.5, `low` /
    `medium`). The default was chosen by running the same briefs through Sonnet 5.5 and
    Opus 5.5 (`make llm-try`); see DECISIONS.md.

- How is the LLM swapped out for tests and local development?
  `LLMClient` is an interface with two implementations:
  - `ClaudeClient`: calls the Claude API with `ANTHROPIC_API_KEY`.
  - `MockClient`: no network. Selected when `LLM_PROVIDER=mock` or when
    `ANTHROPIC_API_KEY` is empty, so `docker compose up` works without a key.
  By default the mock returns deterministic fixtures keyed by the brief (the example brief
  maps to a known segment + messages). Tests can instead load a scripted queue of responses
  (e.g. first malformed JSON, then a non-whitelisted field, then a valid answer) to exercise
  the validate → repair → retry loop and its "give up after N" path.

## API
Plain JSON over `net/http`; no auth (single-marketer demo).

| Method & path | Does |
|---|---|
| `POST /api/campaigns` `{brief}` | creates a campaign and runs the audience step |
| `POST /api/campaigns/{id}/audience` `{brief}` | new brief, audience step again; clears the draft |
| `POST /api/campaigns/{id}/acknowledge` | acknowledges unsupported criteria |
| `POST /api/campaigns/{id}/messages` `{feedback?}` | messages step as an SSE stream; `feedback` revises the stored draft |
| `POST /api/campaigns/{id}/audience/refresh` | recounts the audience for today (no Claude call) |
| `POST /api/campaigns/{id}/approve` `{content}` | validates the final content and queues the messages |
| `GET /api/campaigns`, `GET /api/campaigns/{id}` | read |
| `GET /api/campaigns/{id}/events` | live delivery counts as an SSE stream (see Monitoring) |
| `GET /metrics` | queue depth, message counts, LLM numbers, worker health (see Monitoring) |
| `GET /healthz` | DB ping |

- The messages stream is a `POST` (it creates content), read by the frontend with
  `fetch`; `EventSource` can only `GET`. Preconditions (audience exists, unsupported
  criteria acknowledged, reach > 0, campaign not approved) are checked before the stream
  starts and fail as ordinary JSON errors.
- The final `validated` / `invalid` event is sent only after the draft is stored, so a
  client that reloads on it finds the content. A client that disconnects cancels the
  model call and nothing is stored.
- One LLM step per campaign at a time: a second generation, or an audience change during
  one, gets `409 busy`. The lock is in memory, which holds because the API runs as one
  replica.
- An LLM outcome is not an HTTP error. An unmappable brief, an invalid audience after
  repairs, or Claude being unavailable come back as `200`/`201` with `audience_error`
  (or an `error` event in the stream); HTTP errors are for bad requests (`400`, `404`,
  `409`, `422`) and bugs (`500`). A transient failure (Claude busy) leaves the campaign
  unchanged; a brief-related failure stores the new brief and clears the old audience.
- A new audience resets the campaign to `draft` and clears the draft and the
  acknowledgement: lengths, wording and the unsupported list all depend on the segment.
- The runtime image has no shell or curl, so the compose healthcheck runs
  `/app healthcheck`, which calls `/healthz` (see Monitoring for the other services).

## Sending
- What happens when the marketer clicks "approve"?
  The approve request carries the final (possibly edited) content. It runs through the same
  content validator used for Claude's output — length limits, known placeholders only,
  no empty fields. On failure the API returns `422` with per-field errors and nothing is
  queued. The frontend applies the same rules only for instant feedback; the backend is the
  authority, since the API can be called directly.

  Preconditions, each a `409`/`422` that changes nothing: the campaign is not approved
  yet (`already_approved`), it has an audience, unsupported criteria are acknowledged,
  and the audience was counted today (`stale_audience`, see below). A generated draft
  is not required: the content is validated the same way whoever wrote it, and a draft
  left invalid after repairs can be fixed by hand and approved from status `draft`.

  Personalization is rendered at enqueue time, for every actual recipient (not estimated),
  and the rendered text is stored in `messages.body` / `subject`. The row is then exactly
  what gets sent: auditable, deterministic even if user data changes later, and the worker
  never needs to know about templates. Rendering happens in Go (`content.RenderFor`),
  inside the approve transaction: recipients are read, rendered, and written with one
  `INSERT … SELECT FROM unnest(…) ON CONFLICT DO NOTHING`. Doing it in SQL would need a
  second implementation of the empty-name rule that the Go tests don't cover.
  - Empty `first_name`: the placeholder is removed and the line tidied: spaces collapsed,
    no space before punctuation, and a line that started with the name loses the
    punctuation after it and is capitalized with Turkish rules (`i` → `İ`):
    `"Merhaba {{first_name}}, sepetin…"` → `"Merhaba, sepetin…"`,
    `"{{first_name}}, sepetindekiler…"` → `"Sepetindekiler…"`. Weak spot, accepted:
    `"Sevgili {{first_name}}!"` → `"Sevgili!"`.
  - Over the limit: can't happen for valid content, because the validator measures every
    `{{first_name}}` as the longest name in the audience and that is a hard rule. The
    price: one long name lowers everyone's SMS budget (15 characters in the seed). At
    enqueue each recipient is still checked as a safety net for data that changed after
    the count; a field over its limit gets the version without the name (SMS; for email
    only the subject, the body has no limit). There is no preview screen and no
    fallback/skip choice; `over_limit_policy` stays at its default `fallback`.
  - Language: messages are written in Turkish for now, whatever the brief's language. The
    language is an explicit value (prompt target, `campaigns.content_language`, render
    capitalization), not "the brief's language", so per-recipient language variants can
    be added later without changing the flow (see Later).
  The API then computes the final segment at the stored `as_of`, applies the mandatory
  per-channel consent filter (see Data — the same filter is used for the reach count shown
  in the audience step, so preview and actual send counts match), then inserts one row per
  (user, channel) into the `messages` table with status `pending`. This table IS the
  queue — no separate queue system. If nobody is reachable any more (consent withdrawn
  since the count), approval is rejected with `422 empty_audience` and nothing changes.
  The response is the campaign plus a summary: messages queued per channel and how many
  recipients got the version without their name.

  Stale audience: the day window is pinned to `as_of`. If `as_of` is on an earlier
  (Istanbul) day than the approval, approval returns `409 stale_audience`;
  `POST /api/campaigns/{id}/audience/refresh` recounts the stored segment for today
  without calling Claude, keeps the draft and the acknowledgement, and re-validates the
  draft in case the longest name grew (an invalid draft goes back to `draft` with its
  errors).

  Approve is idempotent through a conditional state transition, all in one transaction:
  ```sql
  UPDATE campaigns
  SET status = 'approved', approved_at = now(), sms_content = $2, email_subject = $3,
      email_body = $4, over_limit_policy = $5, updated_at = now()
  WHERE id = $1 AND status = 'ready'
  RETURNING id;
  -- 0 rows → already approved (or not ready): return 409, write nothing

  INSERT INTO messages (campaign_id, user_id, channel, to_address, subject, body, status)
  SELECT ...   -- segment + consent filter + rendered content
  ON CONFLICT (campaign_id, user_id, channel) DO NOTHING;
  ```
  The `UPDATE` row-locks the campaign, so a concurrent second approve (double click, network
  retry, second tab) waits, then sees `status <> 'ready'` and gets `409` — it can't
  overwrite the campaign content with something different from what was queued.
  `ON CONFLICT DO NOTHING` and the `UNIQUE` constraint remain as second and third layers.
  The frontend shows `409` as "already approved".
  The worker picks up `pending` rows and sends them; status moves to `sent` or `failed`
  (see the retry strategy below for intermediate states).

- How does the worker claim rows, and what if it crashes mid-send?
  Each channel runs its own loop with its own claim query, its own pool of send
  goroutines (`WORKER_CONCURRENCY_PER_CHANNEL`, default 32) and its own rate limiter. A
  loop claims only when its pool has free slots, and at most that many rows (capped by
  `WORKER_BATCH_SIZE`, default 20), so claimed-but-unsent rows never pile up in memory.
  The pool is what makes 100 msg/s reachable: with 50–300 ms provider latency, 5–30
  sends are in flight per channel, while one sequential sender would manage about 5/s.

  Why one loop per channel: rows queued by the same approve share the same
  `next_attempt_at`, so a shared `ORDER BY next_attempt_at LIMIT n` claim returns them in
  no particular order. A batch could be almost all email while the SMS pool idles, and an
  email channel paused by a 429 would hold the SMS rows of the same batch until their
  lease ran out. With separate loops, a pause or a backlog on one channel is invisible to
  the other.

  The claim runs in a short transaction; the HTTP call happens outside it:
  ```sql
  UPDATE messages SET status = 'sending', locked_until = now() + $lease,
                      attempts = attempts + 1, updated_at = now()
  WHERE id IN (
    SELECT id FROM messages
    WHERE channel = $1 AND status IN ('pending', 'retry_scheduled')
      AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED
  )
  RETURNING *;
  ```
  A second, identical claim runs with `WHERE channel = $1 AND status = 'sending' AND
  locked_until < now()` (lease expired: the worker died). The two conditions are kept as
  separate queries instead of one `OR`, so each can use its own partial index (see Data →
  Indexes). `SKIP LOCKED` keeps concurrent claimers from grabbing the same row. If the
  worker crashes after claiming, the lease expires and the row is picked up again. Because
  the same `message_id` is reused on that attempt, the provider deduplicates it — together
  this gives "at-least-once attempt, at-most-once send".

  The result is written with a fenced update:
  ```sql
  UPDATE messages SET status = $3, ... WHERE id = $1 AND status = 'sending' AND attempts = $2;
  ```
  `attempts` as read at claim time is the fencing token. If the lease expired and the row
  was claimed again, `attempts` has moved on, the late result updates 0 rows and is
  dropped. Without it, a late `retry_scheduled` could overwrite a `sent`.

  The lease (`WORKER_LEASE`, 30 s) has to outlast everything a claimed row waits for: the
  HTTP timeout (3 s) and the limiter (a few hundred ms with small batches). A channel
  pause can be longer, so a pause never holds claimed rows: a goroutine that finds its
  channel paused hands the row back (`retry_scheduled` at the end of the pause, attempt
  refunded, same fenced update) instead of sleeping on it.

- What is the retry strategy?
  | Provider answer | Message becomes | Attempt | Channel |
  |---|---|---|---|
  | `200` | `sent` (`provider_message_id`, `sent_at`) | counted | — |
  | `200` with `duplicate: true` | `sent`, same as above | counted | — |
  | `400` / other 4xx | `failed`, no retry | counted | — |
  | `409 in_progress` | `retry_scheduled`, backoff | counted | — |
  | `429` | `retry_scheduled` at now + `Retry-After` | refunded | paused for `Retry-After` |
  | 5xx, timeout | `retry_scheduled`, backoff | counted | — |
  | connection refused / DNS failure | `retry_scheduled` at the end of the pause | refunded | paused 1, 2, 4 … 30 s; reset by the next success |

  - Backoff: base 1 s (`WORKER_BACKOFF_BASE`), doubling per counted attempt, with equal
    jitter: the wait is between d/2 and d. Same helper as the LLM transport retries.
  - At most 5 counted attempts (`WORKER_MAX_ATTEMPTS`); a retryable failure on the last one
    makes the message `failed` with `last_error`.
  - A 429 and an unreachable provider don't count, because they say nothing about the
    message. The worker can't tell why the provider throttled (its own rate limit or load),
    so every 429 gets the same reaction. A refused connection never reached the provider,
    so the message certainly wasn't sent; without the refund, a 20-second outage would
    burn all five attempts (1 + 2 + 4 + 8 s of backoff) and fail the whole campaign. 5xx
    and timeouts do count, because the request may have arrived. Accepted consequence: a
    message can be rescheduled indefinitely while the provider throttles or is down; it
    stays visible as `retry_scheduled`.
  - The attempt is counted at claim and refunded on these outcomes (`attempts - 1` in the
    fenced update), rather than counted when the result is written: a message that
    crashes the worker on every attempt then still runs out of attempts.
  - `409 in_progress` is the one 4xx that isn't permanent (see Mock provider). It counts:
    it only happens when a lease or the HTTP timeout ran out while the provider was still
    working, which is rare, and a refunded path that never ends isn't worth it there.
  - The next attempt time is stored in `next_attempt_at`; the worker only picks rows whose
    `next_attempt_at <= now()`.
  - State machine:
    ```
    pending → sending → sent
                 ↓
           retry_scheduled → sending → … → failed (attempts exhausted or permanent error)
    ```

- How do you prevent the same message from going to the same person twice?
  Two layers:
  1. `UNIQUE (campaign_id, user_id, channel)` on the `messages` table — the row can only
     exist once, so it can't be queued twice even if "approve" is clicked twice.
  2. The message row's id is sent to the mock provider as `message_id` on every attempt,
     including retries. The provider treats a repeated `message_id` as already handled, so
     a retry after a timeout — where you don't know if the first attempt actually went
     through — can't cause a duplicate send.

- How is the 100 msg/s per channel limit enforced?
  A single worker replica runs one in-memory token bucket per channel
  (`golang.org/x/time/rate`, 100 tokens/s, burst 1). Every send to the provider waits on
  its channel's limiter first, so SMS and email are throttled independently, and burst 1
  means no 1-second window ever holds more than 100 sends (plus one at the boundary).
  On a 429 the worker respects `Retry-After` and pauses that channel for that duration.
  The provider limits with the same algorithm (see Mock provider) rather than a fixed
  one-second window. A fixed window and a token bucket measure the same rate differently:
  a compliant client with a full bucket can put 100 + burst requests into the first
  window, a smooth 100/s stream can land 101 in one window, and counting after the
  injected latency would bunch requests into some windows and starve others. The client
  would get 429s for traffic that keeps the rule. A token bucket with a little tolerance
  (burst 10), counted on arrival, never throttles a client that keeps 100/s, so a 429
  always means something to react to.
  Running only one worker replica is a deliberate choice: an in-memory limiter can't be
  shared across replicas, and horizontal scaling of the worker is out of scope for this case.

## Monitoring
- Health checks, one per service:
  - API `GET /healthz`: `200 {"status":"ok","db":"ok"}` after a DB ping, `503` otherwise.
  - Worker `GET /healthz` on `WORKER_HEALTH_ADDR` (`:8081`, inside the compose network
    only): `200` when the DB answers a ping and each channel loop has completed an
    iteration (with or without work) in the last 10 s; `503` otherwise, so a stuck loop
    shows up, not just a dead process.
  - Mock provider `GET /healthz`: `200`.
  - The runtime image has no shell or curl; compose healthchecks run
    `/app healthcheck <url>`.
- Metrics: `GET /metrics` returns plain JSON computed from the DB — no Prometheus — plus
  the worker's health, which the API fetches from the worker's `/healthz` (1 s timeout;
  `unreachable` if it doesn't answer), so the dashboard can show it:
  ```json
  {
    "queue_depth": {"sms": 120, "email": 340},
    "messages": {"sms": {"sent": 900, "failed": 3, "retry_scheduled": 12}, "email": {"...": 0}},
    "llm": {"calls": 42, "validation_failures": 5},
    "worker": {"status": "ok"}
  }
  ```
  Simple to consume from the dashboard and needs no extra library or container.

- How do worker status changes reach the frontend live?
  The API and worker are separate containers and don't talk directly; Postgres is the
  shared source of truth. For each open SSE connection (`GET /api/campaigns/{id}/events`)
  the API polls aggregated counts about once per second:
  ```sql
  SELECT channel, status, count(*) FROM messages WHERE campaign_id = $1 GROUP BY 1, 2;
  ```
  and pushes an event to the client only when the counts changed. The stream closes once the
  campaign has no `pending` / `sending` / `retry_scheduled` rows left.
  Chosen over `LISTEN/NOTIFY`: no extra connection management or reconnect logic, and a
  ~1s delay is fine for a dashboard. The aggregate query uses the index on
  `messages (campaign_id, channel, status)`.

## Testing
`make test` needs only Docker; no API key, no running compose stack.
- Unit tests: content validator (limits counted in runes, placeholders), segment validator
  and query builder (generated SQL + parameters, rejected fields/ops/values, city
  normalization), template rendering and empty-name fallback, retry classification and
  backoff, rate limiter, LLM repair loop and give-up path (scripted `MockClient`), mock
  provider (dedup, `in_progress`, failures not cached, throttle windows, invalid
  recipients).
- Integration tests: testcontainers-go starts a throwaway Postgres per test package. Covers
  claim with `SKIP LOCKED` and lease expiry, fenced result writes, idempotent approve under
  concurrent requests, consent filtering, and an end-to-end run against an in-process mock
  provider (fixed `PROVIDER_SEED`) with high error, timeout and throttle rates, asserting
  that every message ends `sent` / `failed` / `skipped`, that `/deliveries` lists each
  `message_id` at most once and matches the `sent` rows, and that the invalid recipients
  end `failed` after one attempt. A second run lowers the provider's rate limit below the
  worker's to exercise rate-limit 429s.
- Chosen over the compose Postgres so tests are isolated and don't depend on stack state;
  chosen over unit-only because the reliability guarantees live in Postgres behavior
  (`SKIP LOCKED`, `ON CONFLICT`) that mocks can't prove.

## Mock provider
`./app mock-provider`, same binary as the API and worker, its own container on
`PROVIDER_ADDR` (`:8090`, also published to the host so `/deliveries` can be opened in a
browser). No authentication. Synchronous: each request returns sent or an error
immediately; no delivery webhooks.

- Endpoints:
  ```
  POST /sms    {"message_id": "1842", "to": "+905321234567", "body": "..."}
  POST /email  {"message_id": "1843", "to": "ayse.yilmaz.17@example.com",
                "subject": "...", "body": "..."}
  GET  /deliveries
  GET  /healthz
  ```
  `message_id` is a string (our `messages.id`), treated as opaque.

  | Outcome | Status | Body |
  |---|---|---|
  | Sent | `200` | `{"provider_message_id": "sms_9f2c…", "duplicate": false}` |
  | Same `message_id` already sent | `200` | same `provider_message_id`, `"duplicate": true` |
  | Invalid recipient | `400` | `{"error": "invalid_recipient", "message": "…"}` |
  | Missing or malformed field | `400` | `{"error": "invalid_request", "message": "…"}` |
  | Same `message_id` still being processed | `409` | `{"error": "in_progress"}` |
  | Throttled | `429` + `Retry-After: <seconds>` | `{"error": "rate_limited"}` |
  | Server error | `500` / `503` | `{"error": "provider_error"}` |

- Order of checks per request: validate the request → idempotency → rate limit and
  throttle window → injected failure → latency → record the delivery. A duplicate is
  answered without consuming a rate-limit token.
- Idempotency by `message_id`, kept in memory as `in_progress` or `sent`:
  - A new id is marked `in_progress`. On success it becomes `sent` with its
    `provider_message_id`; on any failure (5xx, 429) the entry is removed, so failures are
    never cached and the next attempt is processed normally.
  - A repeated id that is `sent` returns the original answer with `duplicate: true`; one
    that is `in_progress` returns `409 in_progress`.
  - Limitation: a provider restart forgets the ids. Accepted for a mock.
- Invalid recipients (permanent `400`), fixed rather than random so they are predictable
  and appear in every demo: an SMS number that isn't a Turkish mobile
  (`^\+905\d{9}$`), an email address whose domain ends in `.invalid` (RFC 2606, never
  real). The seed includes one user of each (see Data).
- Rate limit: a token bucket per channel, `PROVIDER_RATE_PER_CHANNEL` (100/s) with burst
  10, counted when the request arrives, before the injected latency. Exceeding it
  returns `429` with `Retry-After: 1`.
- Throttle windows: random, per channel, on average every `PROVIDER_THROTTLE_EVERY`
  (30 s) for `PROVIDER_THROTTLE_FOR` (2 s). During a window every request on that channel
  gets `429` with `Retry-After` set to the rest of the window (rounded up). Real providers
  throttle in episodes, not on independent random requests; a 5% chance per request
  would mean about five 429s a second at 100 msg/s, and since `Retry-After` is at least
  1 s and pauses the channel, the channel would hardly ever send.
- Server errors: `PROVIDER_ERROR_RATE` (5%) of requests return 5xx.
- Timeouts: `PROVIDER_TIMEOUT_RATE` (1%) of requests hang until the client disconnects
  (at most 30 s), so the hang always outlasts the worker's `WORKER_HTTP_TIMEOUT` (3 s)
  whatever it's set to. Half hang before processing (nothing sent, an ordinary retry);
  half record the delivery first and then hang, so the message went out but the worker
  never heard. Its retry gets `200 duplicate: true` and the row becomes `sent` with a
  single delivery: the case the idempotency key exists for.
- Latency: uniform between `PROVIDER_LATENCY_MIN_MS` and `PROVIDER_LATENCY_MAX_MS`
  (50–300 ms).
- `PROVIDER_SEED` fixes the random source for tests; empty means random.
- `GET /deliveries`: totals per channel (requests, deliveries, duplicates answered,
  rejections by reason) and the list of delivered messages (`message_id`, channel, `to`,
  `provider_message_id`, `delivered_at`, `requests` = how many times that id was
  requested). Proof that retries didn't turn into duplicate sends.

## Scope
- Bonus features (scheduled sending, A/B variants, content in the user's language, CI)
  are out of scope for now; the core flow comes first.

## Later
- Message language. Today the messages are always written in Turkish (an explicit
  `content_language`, see Sending). English-preferring users (`users.language = en`)
  therefore get Turkish messages. Planned, in two parts:
  - Single-language segments (`language eq en` / `eq tr`): tell the messages prompt to
    write in that language instead of the brief's. A prompt change only.
  - Mixed audiences: translate the final draft once per language (never per user: one
    extra call per campaign, and every text sent is one the marketer approved). The
    messages answer reports its language; if the segment has recipients of the other
    language, the template is translated as its own step with the same schema,
    validator and repair loop, and each recipient gets the variant matching
    `users.language` at enqueue.
  - Needs: per-language content storage (`campaign_contents (campaign_id, language,
    sms, email_subject, email_body)` instead of the content columns on `campaigns`),
    TR/EN tabs in the approval UI with both variants editable, and a staleness rule: if
    the source text is edited after translation, the translation is marked outdated and
    approval is blocked until it is translated again. Translation therefore runs right
    before approval, once the source draft is final.
