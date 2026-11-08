-- +goose Up
ALTER TABLE campaigns
    -- Reference time of the audience preview; reused at approval so the day window
    -- can't move in between (DECISIONS.md).
    ADD COLUMN audience_as_of              timestamptz,
    ADD COLUMN audience_total_count        integer,
    -- Longest first_name in the segment: content lengths are checked against it.
    ADD COLUMN longest_first_name          integer,
    -- Parts of the brief the DSL couldn't express; must be acknowledged before the
    -- messages step when not empty.
    ADD COLUMN unsupported_criteria        jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN unsupported_acknowledged_at timestamptz,
    -- Validation errors of the stored draft; NULL when it is valid (status 'ready').
    ADD COLUMN content_errors              jsonb;

ALTER TABLE llm_calls
    -- The model that answered; differs from the configured one after a refusal fallback.
    ADD COLUMN model text,
    -- Transport error or refusal when no usable answer arrived.
    ADD COLUMN error text;

CREATE INDEX llm_calls_campaign_idx ON llm_calls (campaign_id);

-- +goose Down
DROP INDEX llm_calls_campaign_idx;
ALTER TABLE llm_calls DROP COLUMN error, DROP COLUMN model;
ALTER TABLE campaigns
    DROP COLUMN content_errors,
    DROP COLUMN unsupported_acknowledged_at,
    DROP COLUMN unsupported_criteria,
    DROP COLUMN longest_first_name,
    DROP COLUMN audience_total_count,
    DROP COLUMN audience_as_of;
