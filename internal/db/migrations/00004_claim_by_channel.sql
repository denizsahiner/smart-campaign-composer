-- +goose Up
-- The worker claims per channel (one loop each), so the claim indexes lead with channel.
DROP INDEX messages_claimable_idx;
DROP INDEX messages_lease_idx;
CREATE INDEX messages_claimable_idx ON messages (channel, next_attempt_at) WHERE status IN ('pending', 'retry_scheduled');
CREATE INDEX messages_lease_idx     ON messages (channel, locked_until)    WHERE status = 'sending';

-- +goose Down
DROP INDEX messages_claimable_idx;
DROP INDEX messages_lease_idx;
CREATE INDEX messages_claimable_idx ON messages (next_attempt_at) WHERE status IN ('pending', 'retry_scheduled');
CREATE INDEX messages_lease_idx     ON messages (locked_until)    WHERE status = 'sending';
