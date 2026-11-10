-- +goose Up
-- Why a failed message failed: 'permanent' when the provider rejected it (an invalid
-- recipient: sending again can never help) or 'retries_exhausted' when temporary errors
-- (5xx, timeouts) used up its attempts. Set exactly when status is 'failed'. The campaign
-- page lists the permanent ones: those are the recipients whose data needs fixing.
ALTER TABLE messages ADD COLUMN failure text CHECK (failure IN ('permanent', 'retries_exhausted'));

-- Existing failures: the worker stored the provider's answer, and only a 4xx is permanent.
UPDATE messages
SET failure = CASE WHEN last_error ~ '^4[0-9]{2} ' THEN 'permanent' ELSE 'retries_exhausted' END
WHERE status = 'failed';

ALTER TABLE messages ADD CONSTRAINT messages_failure_status_check CHECK ((status = 'failed') = (failure IS NOT NULL));
CREATE INDEX messages_permanent_failures_idx ON messages (campaign_id, id) WHERE failure = 'permanent';

-- +goose Down
ALTER TABLE messages DROP COLUMN failure;
