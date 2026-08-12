-- MYR-522: an owner may cancel an accepted/arrived ride. `cancelled_by`
-- records WHICH PARTY initiated a cancellation ('rider' | 'owner') so the
-- rider's client can render the honest "had to cancel" copy from a refetch
-- rather than inferring it from "not my own tap". NULL on every row that is
-- not cancelled, and on rows cancelled before this migration (consumers MUST
-- treat absence as "initiator unknown", never guess).
ALTER TABLE go_ride_requests ADD COLUMN cancelled_by text;
