-- A show can have a second source feed (the publisher's premium/bonus
-- feed) melded into the same delayed feed. The premium feed needs its
-- own conditional-GET validators, and each episode needs to remember
-- which feed it came from.
ALTER TABLE subscriptions ADD COLUMN premium_source_url TEXT;
ALTER TABLE subscriptions ADD COLUMN premium_etag TEXT;
ALTER TABLE subscriptions ADD COLUMN premium_last_modified TEXT;

-- episodes.source_role, plus a widened GUID constraint: premium feeds
-- are often ad-free re-cuts of the same episodes, so the same GUID can
-- legitimately appear in both feeds and must stay two rows. SQLite
-- can't alter a UNIQUE constraint, hence the rebuild.
DROP INDEX idx_episodes_feed;
ALTER TABLE episodes RENAME TO episodes_old;

CREATE TABLE episodes (
  id                INTEGER PRIMARY KEY,
  subscription_id   INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
  guid              TEXT NOT NULL,
  source_role       TEXT NOT NULL DEFAULT 'primary',
  position          INTEGER NOT NULL,
  scheduled_at      TIMESTAMP NOT NULL,
  locked            INTEGER NOT NULL DEFAULT 0,
  excluded          INTEGER NOT NULL DEFAULT 0,
  original_pub_date TIMESTAMP,
  title             TEXT NOT NULL,
  description       TEXT,
  link              TEXT,
  enclosure_url     TEXT NOT NULL,
  enclosure_type    TEXT,
  enclosure_length  INTEGER,
  duration          TEXT,
  episode_number    INTEGER,
  season            INTEGER,
  episode_type      TEXT,
  explicit          INTEGER,
  image_url         TEXT,
  first_seen_at     TIMESTAMP NOT NULL,
  missing_since     TIMESTAMP,
  UNIQUE (subscription_id, source_role, guid),
  UNIQUE (subscription_id, position)
);

INSERT INTO episodes (
  id, subscription_id, guid, source_role, position, scheduled_at, locked, excluded,
  original_pub_date, title, description, link, enclosure_url,
  enclosure_type, enclosure_length, duration, episode_number, season,
  episode_type, explicit, image_url, first_seen_at, missing_since
)
SELECT
  id, subscription_id, guid, 'primary', position, scheduled_at, locked, excluded,
  original_pub_date, title, description, link, enclosure_url,
  enclosure_type, enclosure_length, duration, episode_number, season,
  episode_type, explicit, image_url, first_seen_at, missing_since
FROM episodes_old;

DROP TABLE episodes_old;

CREATE INDEX idx_episodes_feed ON episodes (subscription_id, scheduled_at);
