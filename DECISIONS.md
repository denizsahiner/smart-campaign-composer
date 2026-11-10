# Decisions

Key decisions and their rationale. The full design is in
[docs/ARCHITECTURE_DRAFT.md](docs/ARCHITECTURE_DRAFT.md); this file records the choices
where a reasonable alternative existed, and what we gave up.

## LLM output

### The LLM client returns raw output; validation lives in the caller

`llm.Client` returns the model's structured output as unparsed bytes. Parsing,
validation, repair and retries happen in the caller, through the same validators for
Claude and the mock.

- **Why:** the most valuable tests feed the system malformed output (truncated JSON,
  `"30"` as a string, extra keys, trailing text). A client that returned typed structs
  couldn't produce those from the mock, so the repair path would only ever run against
  the real API. It also keeps strict decoding in one place, keeps the exact raw text for
  `llm_calls.raw_response`, and means a new step needs a schema and a validator, not a
  change to the client interface.
- **Alternatives:** typed struct per step, a generic `Generate[T]`, `map[string]any`
  (loses integer vs. float and unknown-key detection), `json.RawMessage` (implies valid
  JSON, which this explicitly may not be).
- **Trade-off:** typed results appear one layer up (the validator) instead of at the
  client boundary.

### Default model: Claude Sonnet 5.5

Chosen by running six briefs through Sonnet 5.5 and Opus 5.5 with the same prompts
(`make llm-try`, default efforts `low` for audience and `medium` for messages): the
example brief in Turkish and English, a city that isn't in the data (Paris), criteria
the DSL can't express (age, gender), several cities plus a language, and a brief packed
with offers to push the SMS length.

| | Sonnet 5.5 | Opus 5.5 |
|---|---|---|
| Valid on first answer | 5/5, no repairs | 5/5, no repairs |
| Paris brief | reported as unmappable | reported as unmappable |
| Audience step latency | 1.2–1.7 s | 1.6–2.1 s |
| Messages step latency | 2.8–4.8 s | 5.8–15.8 s |
| Messages output tokens (avg) | ~370 | ~860 |
| Cost of the six briefs | ~$0.05 | ~$0.16 |

Segments and unsupported criteria were identical, and copy quality was comparable
(Opus wrote longer emails). The marketer waits on screen, so Sonnet's 2–3x faster
messages step decides it. The model is configuration (`ANTHROPIC_MODEL`), so switching
back is a setting, not a code change.

Limits of the evidence: six briefs, one run each, and the repair path never triggered
against the real model (it is covered by mock tests).

### Structured outputs, not forced tool use

Each step's JSON Schema is sent as `output_config.format`. The original plan was a tool
with `tool_choice` forcing the call; Claude Opus 5.5 rejects forced tool use with a 400,
and structured outputs exist for exactly this "I just want JSON back" case. The answer
arrives as JSON text, so streaming needs no tool-input reassembly.

Structured outputs don't support `minimum`/`maximum`/`minLength`/`maxLength`. The schema
guarantees shape; character limits and the 1–365 day range are enforced only by the
validators.

### Cities are a free string; unmappable criteria are reported, not guessed

Constrained decoding makes the model produce something the schema allows. With cities
as an enum, a brief about "Paris" would be forced onto a known city: a wrong segment,
silently. So cities are a free string checked by the validator, and the schema has an
`unsupported_criteria` list where the model puts parts of the brief the DSL can't express
(a city not in the data, age, order value, email opens). An answer with no rules but a
reason is final, not a repair case: the marketer has to change the brief.

When the list is not empty, the marketer must explicitly acknowledge it before the
messages step; the API enforces this, not just the UI. A dropped criterion widens the
audience rather than narrowing it: "cart abandoners in Paris" becomes every cart
abandoner in every city, so a passive warning isn't enough.

### Repair rounds re-send the original request with a quoted answer

A repair round is the original request plus the previous answer and its validation
errors in the user turn; the model's previous turn is not replayed. Current models bind
thinking blocks to the turn that produced them, so replaying or editing that turn is
fragile, and starting from the original request keeps every round the same size.

### Retry budgets are per step

At most 2 repair rounds and 2 transport retries per step, so at most 5 calls. Per-call
transport budgets would allow 3 × 3 = 9 calls while the marketer watches a spinner. A
`retry-after` over 10 s fails at once with "Claude is busy" instead of waiting.

### Exhausted repairs hand the draft to the marketer

If the messages are still invalid after the repair attempts (most likely an SMS a few
characters too long), the draft is shown editable with the failing fields marked, and the
approve button stays disabled until it passes validation. The approve endpoint runs the
same validator, so this is safe, and the marketer, who is in the loop anyway, can cut 8
characters faster than starting over. Failing outright is reserved for having no usable
answer at all.

The audience step has no such fallback: a segment isn't something the marketer edits by
hand, so the errors are shown and the brief is edited instead.

## Audience

### Day windows are calendar days in Europe/Istanbul

`cart_added_days_ago` counts calendar days in Istanbul time: today is 0, yesterday is 1.
"Last 30 days" with a reference time of Nov 7 14:00 starts at Oct 8 00:00 Istanbul
(Oct 7 21:00 UTC). Boundary instants are computed in Go and passed to SQL as
`timestamptz` parameters.

- **Why:** this is what a marketer means by "last 30 days". A rolling window
  (`now() - 30 days`) would make the audience depend on the hour the campaign is built.
- **Alternatives:** rolling 24h windows (simpler, timezone-free); calendar days in UTC
  (wrong by three hours for Turkish users).
- **Tested:** hand-placed events at exact boundaries (00:00:00.000000 vs. the previous
  microsecond, a UTC date that is the next day in Istanbul), plus a comparison against a
  Go reference over the whole seed. Deliberately broken versions of the boundary logic
  were confirmed to fail these tests.

### The reference time is pinned between preview and approval

The time the audience is previewed (`as_of`) is stored and reused at approval, so the
window can't shift in between, e.g. across midnight, when everyone whose last cart was
on the first day of the window would drop out at once. Only the time is pinned: the
segment is re-evaluated at approval, so consent withdrawn in the meantime is respected.
The count queued at approval can therefore differ slightly from the preview, and the
approval response reports the actual count.

## Content

### Character limits are counted in Unicode code points

SMS ≤ 160 and email subject ≤ 80 are counted with `utf8.RuneCountInString`, not bytes:
"ş" is one character. GSM-7 vs. UCS-2 encoding, where a single Turkish character drops
the real per-segment SMS limit to 70, is out of scope; the case's 160-character rule is
applied as stated.

### Length is checked with the longest first name in the audience

Each `{{first_name}}` is measured as the longest first name in the segment, so a
template that passes fits every recipient. The longest name comes from the same query
as the reach count. It is measured over the whole segment, not per channel, which is
slightly conservative.

LLMs count characters poorly, especially in Turkish, so this check fails more often than
others. It is never relaxed for that; instead the prompt asks for a shorter target than
the limit, and the repair message states exactly how much to cut.

### Only `{{first_name}}`, and no other braces

The only placeholder is `{{first_name}}`. Unambiguous variants (`{{ First_Name }}`) are
normalized. Unknown names (`{{name}}`, `{{last_name}}`) are rejected, and so is any brace
left once valid placeholders are removed: `{first_name}`, `{{first_name}`,
`{{{first_name}}}`, and also legitimate-looking text like `Kod: {SEPET30}`. Mentions of
the variable in other syntaxes (`%first_name%`, `[FirstName]`) are rejected too.

- **Why:** the two mistakes don't cost the same. Rejecting legitimate text costs the
  marketer a few seconds of editing, with a clear message. Letting a malformed
  placeholder through sends "Merhaba {first_name}" verbatim to hundreds of people, and
  that can't be undone. Braces are rare in SMS/email marketing copy.
- **Alternative:** reject only brace content that looks like an identifier. `{SEPET30}`
  looks like one too, so this gains little and makes the rule harder to explain.
- **Same rules everywhere:** the model's draft and the marketer's edited content at
  approval go through the same validator.

## Sending

### Over-limit recipients: prevented, not previewed

The architecture draft planned a preview before approval listing recipients whose
rendered message exceeds the limit, with a per-campaign choice to send them the version
without the name or skip them. The content validator already measures every
`{{first_name}}` as the longest name in the audience and enforces it as a hard rule, so
for valid content nobody can exceed the limit and that preview would never show anyone.
The hard rule stays and the preview and choice were dropped. The cost is a tighter
budget for everyone when one name is long. A per-recipient check at enqueue remains as a
safety net for data that changed after the count: an SMS or email subject over its limit
gets the version without the name (the email body keeps it).

### Empty first name: remove the placeholder and tidy the line

`"Merhaba {{first_name}}, sepetin…"` → `"Merhaba, sepetin…"`, and a line that started
with the name is capitalized (`"{{first_name}}, sepetindekiler…"` → `"Sepetindekiler…"`).
Chosen over a neutral word ("değerli müşterimiz"), which reads artificially and makes the
text longer, and over asking Claude for a second, name-less variant of every field, which
doubles the schema, validation and approval UI. Weak spot, accepted:
`"Sevgili {{first_name}}!"` → `"Sevgili!"`.

### Messages in each recipient's language, written in one call

Every recipient gets the messages in their preferred language (`users.language`: Turkish
or English), whatever language the brief is written in. A campaign keeps one draft per
language (`campaign_contents`), and only for the languages its audience has a reachable
recipient in: nobody reads, checks or approves a text that would go to no one. The
audience count is kept per language, so each draft's length limits are measured with
the longest name among its own recipients.

All languages are written in one call, as one JSON object keyed by language. The
alternatives were a call per language, or Turkish first and then a translation. One call
keeps the versions saying the same thing (same offer, same tone) and keeps one stream and
one repair loop; the prompt asks for each language to be written natively, not as a
translation of the other. Revising picks the languages to rewrite: the others are sent as
context and kept as they are, so a fix to the English text can't change a Turkish text
the marketer already checked, and an edit in one tab survives a revision of the other.

A language can gain its first reachable recipient when the audience is refreshed; the
campaign then stays a draft until that language is written. If a recipient's language
still has no text at approval (an audience counted before per-language counts), the
approval is refused rather than sending them another language.

### Rendering at enqueue happens in Go

The draft planned a single `INSERT … SELECT`. The empty-name rule (capitalization,
punctuation) would need a second implementation in SQL that the Go tests don't cover, so
recipients are read, rendered by `content.RenderFor` and inserted in one statement, all
inside the approve transaction.

### Approval rules

- A stale audience (counted on an earlier day) must be refreshed first; refresh recounts
  without calling Claude and keeps the draft.
- Hand-written content can be approved without a generated draft, and an invalid draft
  fixed by hand can be approved from status `draft`; the same validator decides.
- If nobody is reachable at approval time, approval is rejected (`422`) rather than
  creating an approved campaign with no messages.
- The response reports messages queued per channel and how many recipients got the
  version without their name.

### Mock provider: synchronous, with configurable failures

The mock SMS/email provider answers each request immediately (sent or an error) and has
no delivery webhooks. Its failures (5xx, throttling with `Retry-After`, timeouts,
latency) are configurable, with non-zero defaults so every run exercises retries and rate
limiting; it enforces its own 100/s per channel and deduplicates by `message_id`. A
delivered/failed webhook would be more realistic but adds an endpoint and states without
changing what the case asks us to show: retries, rate limiting and no duplicates.
`GET /deliveries` lists what was actually delivered and how often each id was requested,
which is the evidence that retries didn't become duplicate sends.

### Throttling comes in windows, and every 429 gets the same reaction

The first plan was a 5% chance of 429 on each request. At 100 msg/s that is about five
429s a second; `Retry-After` is at least one second and pauses the channel, so the
channel would hardly ever send. Treating injected 429s differently from rate-limit 429s
would hide the problem instead of fixing it: a real client can't tell why it was
throttled. So the provider throttles in episodes (on average every 30 s, for 2 s, per
channel), like real providers under load, and the worker handles every 429 the same way:
reschedule the message at `Retry-After`, pause the channel, don't count the attempt.

### The same rate-limit algorithm on both sides

Worker and provider both use a 100/s token bucket per channel: burst 1 on the worker,
burst 10 on the provider, counted when the request arrives. With a fixed one-second
window on the provider, a client keeping 100/s would still see 429s (a full bucket at
start, 101 requests landing in one window, latency bunching arrivals). A client that keeps
the rule now never hits the provider's limit; tests lower the provider's limit to
exercise that path.

### What counts as an attempt

At most 5 attempts per message. A 429 or a refused connection does not count: neither
says anything about the message, and a refused connection means it certainly wasn't
sent. Without this, a 20-second provider outage would exhaust every message's attempts
and fail the whole campaign. 5xx, timeouts and `409 in_progress` count, because the
request may have reached the provider. The attempt is counted at claim and refunded,
rather than counted when the result is written, so a message that crashes the worker
still runs out of attempts. Accepted consequence: a message can stay `retry_scheduled`
for as long as the provider throttles or is down.

### One claim loop per channel, fenced result writes

Each channel has its own claim query, goroutine pool and limiter, and claims only as
many rows as it has free slots. A shared claim mixed channels in one batch, so one
channel's pause or backlog held the other's rows. Results are written with
`WHERE status = 'sending' AND attempts = <attempt at claim>`, so a result arriving after
the lease expired and the row was claimed again is dropped instead of overwriting the
newer outcome. A paused channel hands its claimed rows back rather than sleeping on them,
so pauses never run into the lease.

### Idempotency in the provider: only successes are remembered

The provider marks a new `message_id` as in progress, keeps it as sent on success, and
forgets it on any failure, so a cached 5xx can't block every later retry. A repeat of a
sent id gets the original answer (`duplicate: true`); a repeat while the first is still
running gets `409 in_progress`. Kept in memory: a provider restart forgets the ids,
accepted for a mock.

### Timeouts: half before, half after the delivery

An injected timeout hangs until the client gives up. Half of them hang before
processing (nothing sent, an ordinary retry); half record the delivery first, so the
worker times out on a message that did go out, and its retry must come back as a
duplicate rather than a second send. That second case is the one the idempotency key
exists for, so the mock makes sure it happens.

### Permanent failures come from fixed recipients

The provider rejects SMS numbers that aren't Turkish mobiles and email domains ending
in `.invalid` with a `400`, and the seed contains one user of each, matching the example
brief. Random 400s would make the permanent-failure path appear by chance; fixed ones
make it appear in every demo and test.

### Rejected recipients are listed on the campaign, from the messages table

A failed message records why: `messages.failure` is `permanent` (the provider rejected
it) or `retries_exhausted` (temporary errors used up the attempts). Only the permanent
ones are listed on the campaign page and in its CSV download: those recipients have bad
data that someone has to fix, while an exhausted retry says nothing about the recipient.
The list reads the messages table, not the logs, so it stays as long as the campaign does
instead of disappearing with log retention, and personal data (addresses) stays out of
the logs. A column was chosen over parsing `last_error`, whose wording is the provider's.
The CSV is made for Turkish Excel: UTF-8 with a BOM, `;` as the separator, and a `'` in
front of values starting with `=`, `+`, `-` or `@` so Excel reads neither formulas nor
phone numbers as numbers.

### Scheduled sends: queued at approval, started by the claim

A campaign can be approved for a later time (`send_at` on approve). Options considered:
build the recipients and texts at send time (fresh audience, but an approved text could
stop fitting a new language or a longer name, and it needs a scheduler process), or at
approval. Chosen: at approval. The messages are queued exactly as for an immediate send,
with `next_attempt_at = scheduled_at`, so what goes out is what the marketer saw, and the
worker needed no new logic to wait. The staleness check stays at approval only; the
14-day limit bounds how old the audience can get.

The campaign has its own `scheduled` status rather than "approved with a future
`scheduled_at`", so lists and queries don't have to compute it. It becomes `approved` in
the worker's claim statement (a data-modifying CTE next to the message claim), not in a
separate polling loop: the first message taken is the moment the send starts, in the same
transaction.

Cancel deletes the pending messages and puts the campaign back to `ready` with its
drafts, so it can be edited and approved again; a `canceled` status that keeps the rows
was rejected because the campaign could not be reused. Cancel deletes messages first and
updates the campaign second, the same lock order as the claim, so the two can't
deadlock. If the claim got a message first, that row is `sending` and the campaign is
`approved`, the update matches nothing and the cancel rolls back with `409
send_started`. Cancels are logged at warn so they show in `app_logs`.

Rules for the time, Istanbul time: at least a minute ahead, at most 14 days, and not
between 21:00 and 09:00 (commercial messages at night). The quiet hours apply to choosing
the time only, not to immediate sends or retries that run past 21:00.

### Worker health is checked, and reported through the API

The worker opens a small `/healthz` that is healthy only if the DB answers and each
channel loop ran in the last 10 seconds, so a stuck loop is caught, not just a dead
process. The dashboard can't reach the worker, so the API's `/api/metrics` includes the
worker's health.

### Equal jitter for every backoff

Backoffs wait between d/2 and d (equal jitter), for Claude calls and for sends alike, from
one helper. Full jitter (0 to d) spreads retries best but can retry almost immediately;
a symmetric ±20% barely spreads them. The per-channel limiter already smooths a herd of
retries, so the difference is small, and one rule is easier to reason about.

## Monitoring

### Metrics live under /api and survive a dependency being down

Metrics are `GET /api/metrics`, next to the rest of the API the frontend calls;
`/healthz` stays at the root, where Docker calls it. Besides the database numbers, the
endpoint fetches the worker's health and the provider's totals live (1 s timeout each,
in parallel). A dependency that doesn't answer is reported as `unreachable` inside a
`200`, rather than failing the whole response: the dashboard's job at that moment is to
show which part is down.

### The delivery stream ends on the campaign's status

`GET /api/campaigns/{id}/events` sends the current counts on connect and then only when
they change, plus a 15 s keep-alive comment. It ends with `done` when the worker marks the
campaign `completed`, which happens exactly when no message is left open, so the final
event carries the final status rather than "approved, nothing left". The send rate is
averaged over the last 5 seconds: smooth enough to read, at the cost of trailing the real
rate when sending starts and stops. Each connection polls on its own; sharing one query
per campaign would only pay off with many viewers.

### Logs: stdout for everything, Postgres for warnings and errors

Every service logs JSON to stdout with a `service` attribute; `docker compose logs` is the
complete record. Warnings and errors of the API and the worker are also written to an
`app_logs` table and shown on a logs page, so a problem can be found from the UI without
a shell. We considered Loki + Grafana: better search and dashboards, but two or three more
containers and their config for a project that should come up with one command. A log
file in a volume would add rotation and still not be searchable from the UI. Slack alerts
notify, they don't store.

Keeping the table cheap and harmless:

- Only warn and error are stored. A request line or a sent message is info/debug: at 100
  sends a second per channel they would be most of the writes and none of the value.
- The sink buffers records and inserts them in batches (COPY, every second or 200 rows)
  on its own goroutine. When the buffer is full, records are dropped and the drop is
  reported on stdout: logging never slows a request or a send.
- If the database is down, its own errors can't be stored; they are still on stdout.
  The sink reports its failures through the stdout handler only, so it never feeds
  itself.
- The API deletes rows older than `LOG_RETENTION_DAYS` (default 7) every hour.

`campaign_id` is how a campaign's story is followed end to end. A request ID can't do it:
the queue sits between the API and the worker. So every log line about a campaign carries
`campaign_id`, from the request that loaded it (added to the request's log scope once the
handler knows the campaign), the LLM steps and the worker's sends. Each request gets a
request ID (a valid incoming `X-Request-ID` is kept), returned in the header and, on a
500, in the error body, which the UI shows as "Hata kodu" for the logs page's filter.

What counts as an error decides what the campaign page flags. With the mock provider's
default 5% errors and 429 windows, retries are routine, so they are warnings; a message
given up on (rejected, or out of attempts), a failed LLM step and a 500 are errors. A
draft that stays invalid or a refusal is a warning: the marketer handles it on screen. A
throttle or an unreachable provider is logged once per channel pause, not once per
message that hit it.

## Frontend

### The API serves the built frontend; no nginx

The Dockerfile builds the Vue app in a Node stage and copies it into the runtime image;
the API serves it when `WEB_DIR` is set, so `docker compose up` brings up the UI at
`http://localhost:8080` with no extra container. The draft planned an nginx `frontend`
service proxying `/api`; that buys a separate cache and TLS layer we don't need for a
local demo, and costs another image, config file and hop for the long-lived SSE streams.
On one origin there's no CORS and no proxy buffering to switch off. Paths that aren't
files get `index.html` so client routes survive a reload, except under `/assets/`, where
a stale bundle name must fail as a 404 rather than load HTML as a script; unknown `/api/`
paths keep the JSON error shape. What we give up: the API process also serves static
files, and a frontend change means rebuilding the API image. Without `WEB_DIR` the API
serves only the API, as in development, where Vite serves the app and proxies `/api`
and `/healthz`.
