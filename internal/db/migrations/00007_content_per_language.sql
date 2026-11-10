-- +goose Up
-- Every recipient gets the messages in their preferred language (users.language), so a
-- campaign has one draft per language: a row per (campaign, language) instead of the
-- content columns on campaigns. errors are the draft's validation errors (paths without
-- the language), NULL when it passes.
CREATE TABLE campaign_contents (
    campaign_id   bigint      NOT NULL REFERENCES campaigns (id),
    language      text        NOT NULL CHECK (language IN ('tr', 'en')),
    sms           text        NOT NULL,
    email_subject text        NOT NULL,
    email_body    text        NOT NULL,
    errors        jsonb,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, language)
);

INSERT INTO campaign_contents (campaign_id, language, sms, email_subject, email_body, errors, updated_at)
SELECT id, content_language, sms_content, email_subject, email_body, content_errors, updated_at
FROM campaigns WHERE sms_content IS NOT NULL;

-- The audience counts per language (segment.Counts.Languages): which languages need a
-- draft and the longest name each draft is measured with. NULL for audiences counted
-- before this migration; they are read as all in the old content language.
ALTER TABLE campaigns
    ADD COLUMN language_counts jsonb,
    DROP COLUMN sms_content,
    DROP COLUMN email_subject,
    DROP COLUMN email_body,
    DROP COLUMN content_errors;

-- +goose Down
ALTER TABLE campaigns
    ADD COLUMN sms_content    text,
    ADD COLUMN email_subject  text,
    ADD COLUMN email_body     text,
    ADD COLUMN content_errors jsonb,
    DROP COLUMN language_counts;

-- One language fits the old shape: Turkish when there is one, else the other.
UPDATE campaigns c SET
    sms_content = cc.sms, email_subject = cc.email_subject, email_body = cc.email_body,
    content_errors = cc.errors, content_language = cc.language
FROM (SELECT DISTINCT ON (campaign_id) * FROM campaign_contents
      ORDER BY campaign_id, language = 'tr' DESC) cc
WHERE cc.campaign_id = c.id;

DROP TABLE campaign_contents;
