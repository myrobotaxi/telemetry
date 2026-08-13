-- Reverses 0039. Dropping the table drops its index with it; the FK's CASCADE
-- lives on this side of the relationship, so go_ride_requests is untouched.
DROP TABLE IF EXISTS go_ride_stops;
