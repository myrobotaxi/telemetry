-- Reverses 0040. Dropping the table drops its primary key and its index with
-- it; the FK's CASCADE lives on this side of the relationship, so
-- go_ride_requests is untouched by that half.
DROP TABLE IF EXISTS go_ride_members;

DROP INDEX IF EXISTS uq_go_ride_requests_join_code;

ALTER TABLE go_ride_requests
    DROP COLUMN IF EXISTS join_code_expires_at,
    DROP COLUMN IF EXISTS join_code,
    DROP COLUMN IF EXISTS group_ride;
