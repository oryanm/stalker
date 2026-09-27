-- When the current run of failed fetches began, 0 while the last fetch succeeded.
ALTER TABLE follows ADD COLUMN failing_since INTEGER NOT NULL DEFAULT 0;

-- the first failure of a running streak is unknown: a short streak counts from its last attempt,
-- a long one is dated a day back so its warning keeps showing
UPDATE follows SET failing_since = iif(error_count >= 3, last_fetched_at - 86400000, last_fetched_at)
WHERE error_count > 0;
