-- The icon found on the follow's home page, the fallback for a feed without an image of its own.
ALTER TABLE follows ADD COLUMN icon_url TEXT NOT NULL DEFAULT '';
-- When the home page was last searched for an icon, 0 means never.
ALTER TABLE follows ADD COLUMN icon_checked_at INTEGER NOT NULL DEFAULT 0;
