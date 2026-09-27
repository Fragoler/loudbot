-- +goose Up

-- Direct Messages turned out to be awkward in testing, so suggestions now arrive
-- as a conversation with the bot. The columns that addressed a message in the
-- channel's DM chat have nothing left to point at.
DROP INDEX suggested_posts_message_idx;

ALTER TABLE suggested_posts
    DROP COLUMN dm_chat_id,
    DROP COLUMN dm_message_id;

ALTER TABLE suggested_posts
    -- The card in the moderation chat, so an edit can be reflected in place and a
    -- decision can retire its buttons.
    ADD COLUMN moderation_message_id INTEGER,
    -- The post itself once it reaches the channel; this is what a link is built from.
    ADD COLUMN channel_message_id INTEGER,
    ADD COLUMN edited_at TIMESTAMPTZ;

-- The bot publishes approved posts itself now, so "failed" is reachable: Telegram
-- can refuse the send after a moderator already said yes.
ALTER TABLE suggested_posts
    ADD CONSTRAINT suggested_posts_status_check
    CHECK (status IN ('pending', 'approved', 'published', 'declined', 'withdrawn', 'failed'));

CREATE INDEX suggested_posts_author_idx ON suggested_posts (user_id, status);


-- What the bot is waiting for in a private chat: the text of a brand new
-- suggestion, or a rewrite of one already on file. Separate from comment_drafts
-- because the two flows ask for different things and must not be confused.
CREATE TABLE suggestion_drafts (
    user_id BIGINT PRIMARY KEY REFERENCES users (user_id) ON DELETE CASCADE,
    -- editing_id is the suggestion being rewritten; NULL means a new one.
    editing_id BIGINT REFERENCES suggested_posts (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE suggestion_drafts;
DROP INDEX suggested_posts_author_idx;

ALTER TABLE suggested_posts DROP CONSTRAINT suggested_posts_status_check;

ALTER TABLE suggested_posts
    DROP COLUMN edited_at,
    DROP COLUMN channel_message_id,
    DROP COLUMN moderation_message_id;

ALTER TABLE suggested_posts
    ADD COLUMN dm_chat_id    BIGINT,
    ADD COLUMN dm_message_id INTEGER;

CREATE UNIQUE INDEX suggested_posts_message_idx
    ON suggested_posts (dm_chat_id, dm_message_id)
    WHERE dm_chat_id IS NOT NULL;
