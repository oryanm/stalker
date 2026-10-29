-- YouTube Shorts are skipped when feeds are parsed; delete the ones stored before that.
DELETE FROM posts WHERE url LIKE 'https://www.youtube.com/shorts/%';

-- the newest post may have been a Short
UPDATE follows SET last_post_at = (SELECT coalesce(max(published_at), 0) FROM posts WHERE follow_id = follows.id);
