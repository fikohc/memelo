-- Single-row storage for the gotd MTProto session (auth key, DC config),
-- so pod restarts reuse the bot authorization instead of logging in again.
CREATE TABLE mtproto_session (
    id smallint PRIMARY KEY,
    data bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Bot API file_id of each meme's media already known to Telegram, used to
-- answer inline queries with cached media instead of URLs (Telegram fetches
-- URLs only up to 5 MB for photos and 20 MB for videos).
CREATE TABLE media_file_cache (
    meme_id uuid PRIMARY KEY,
    type text NOT NULL,
    file_id text NOT NULL,
    file_unique_id text,
    size bigint,
    created_at timestamptz NOT NULL DEFAULT now()
);
