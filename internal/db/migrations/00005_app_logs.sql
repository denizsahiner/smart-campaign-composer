-- +goose Up
-- Warnings and errors of the API and the worker, for the logs page. Everything is also
-- written to stdout; this table is the searchable copy, pruned after LOG_RETENTION.
-- campaign_id has no foreign key: writing a log row must never fail on the data it
-- describes.
CREATE TABLE app_logs (
    id          bigserial   PRIMARY KEY,
    ts          timestamptz NOT NULL,
    level       text        NOT NULL CHECK (level IN ('warn', 'error')),
    service     text        NOT NULL,
    msg         text        NOT NULL,
    request_id  text,
    campaign_id bigint,
    attrs       jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX app_logs_ts_idx       ON app_logs (ts);
CREATE INDEX app_logs_campaign_idx ON app_logs (campaign_id, level) WHERE campaign_id IS NOT NULL;
CREATE INDEX app_logs_request_idx  ON app_logs (request_id) WHERE request_id IS NOT NULL;

-- +goose Down
DROP TABLE app_logs;
