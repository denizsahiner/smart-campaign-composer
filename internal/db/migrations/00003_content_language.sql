-- +goose Up
-- Language the content is written in. Always 'tr' for now; explicit so that
-- per-recipient language variants can be added later (docs/ARCHITECTURE_DRAFT.md, Later).
ALTER TABLE campaigns
    ADD COLUMN content_language text NOT NULL DEFAULT 'tr' CHECK (content_language IN ('tr', 'en'));

-- +goose Down
ALTER TABLE campaigns DROP COLUMN content_language;
