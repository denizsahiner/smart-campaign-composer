-- +goose Up
-- A campaign can be approved for a later time. Its messages are queued at approval as
-- usual, with next_attempt_at = scheduled_at, so the worker picks them up only then; the
-- claim that takes the first one moves the campaign from 'scheduled' to 'approved'.
-- scheduled_at is kept after that, as the time the send was planned for.
ALTER TABLE campaigns DROP CONSTRAINT campaigns_status_check;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_status_check
        CHECK (status IN ('draft', 'ready', 'scheduled', 'approved', 'completed')),
    ADD COLUMN scheduled_at timestamptz,
    ADD CONSTRAINT campaigns_scheduled_at_check CHECK (status <> 'scheduled' OR scheduled_at IS NOT NULL);

-- +goose Down
-- A scheduled campaign has its messages queued already: it becomes an approved one.
UPDATE campaigns SET status = 'approved' WHERE status = 'scheduled';
ALTER TABLE campaigns
    DROP CONSTRAINT campaigns_scheduled_at_check,
    DROP COLUMN scheduled_at,
    DROP CONSTRAINT campaigns_status_check;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_status_check CHECK (status IN ('draft', 'ready', 'approved', 'completed'));
