package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// Ride-request list cursors (MYR-174; second typed view added by MYR-360).
//
// ONE ENCODING ON THE WIRE, TWO TYPED VIEWS OF IT. Every ride-request list
// cursor is the same opaque base64(JSON) `{anchor timestamp, id}` pair; what
// differs between views is WHICH column the anchor timestamp names and which
// DIRECTION the keyset walks:
//
//   - RideRequestListCursor — (createdAt, id), walked DESCENDING. The rider
//     list and the owner incoming feed's default `requested` slice.
//   - RideRequestUpcomingCursor — (scheduledFor, id), walked ASCENDING. The
//     MYR-360 `upcomingForVehicle` slice, which is ordered soonest-first.
//
// The two anchors are separate Go types on purpose: they are not
// interchangeable (feeding an ascending anchor to a descending query would
// silently return the wrong page), and the compiler is a cheaper guard than a
// comment. The JSON key stays `createdAt` for BOTH so a cursor minted before
// MYR-360 still decodes and there is exactly one format to reason about on the
// wire; the cursor is opaque to clients (rest-api.md §4.2.1), so the key name
// is an implementation detail, not a contract.

// RideRequestListCursor is the (createdAt, id) anchor the store resumes a
// DESCENDING keyset scan from. Zero value = first page.
type RideRequestListCursor struct {
	CreatedAt time.Time
	ID        string
}

// RideRequestUpcomingCursor is the (scheduledFor, id) anchor the
// upcoming-reservations slice resumes an ASCENDING keyset scan from. Zero
// value = first page. Every row in that slice has a non-null scheduled_for by
// construction, so the row-value comparison never meets a NULL.
type RideRequestUpcomingCursor struct {
	ScheduledFor time.Time
	ID           string
}

// rideRequestCursor is the opaque base64(JSON) wire cursor. `createdAt` is a
// historical key name; the value is the ANCHOR TIMESTAMP of whichever ordering
// the view uses (see the file doc). It travels as RFC3339Nano so it round-trips
// into the store's timestamptz keyset comparison without precision loss.
type rideRequestCursor struct {
	Anchor string `json:"createdAt"`
	ID     string `json:"id"`
}

// errMalformedRideCursor is the sentinel every recoverable cursor parse
// failure maps to; the handler surfaces it as 400 invalid_request without
// echoing the internal parse error.
var errMalformedRideCursor = errors.New("malformed ride-request cursor")

// encodeRideCursor serialises an (anchor, id) pair into the opaque wire
// cursor. Marshaling two strings cannot fail.
func encodeRideCursor(anchor time.Time, id string) string {
	raw, _ := json.Marshal(rideRequestCursor{
		Anchor: anchor.UTC().Format(time.RFC3339Nano),
		ID:     id,
	})
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeRideCursorAnchor parses the opaque cursor into its raw (anchor, id)
// pair. Returns errMalformedRideCursor on any bad base64 / JSON / field.
func decodeRideCursorAnchor(s string) (time.Time, string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", errMalformedRideCursor
	}
	var c rideRequestCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return time.Time{}, "", errMalformedRideCursor
	}
	if c.Anchor == "" || c.ID == "" {
		return time.Time{}, "", errMalformedRideCursor
	}
	ts, err := time.Parse(time.RFC3339Nano, c.Anchor)
	if err != nil {
		return time.Time{}, "", errMalformedRideCursor
	}
	return ts, c.ID, nil
}

// decodeRideCursor parses an opaque cursor into the DESCENDING (createdAt, id)
// anchor used by the rider list and the default owner feed.
func decodeRideCursor(s string) (RideRequestListCursor, error) {
	anchor, id, err := decodeRideCursorAnchor(s)
	if err != nil {
		return RideRequestListCursor{}, err
	}
	return RideRequestListCursor{CreatedAt: anchor, ID: id}, nil
}

// decodeRideUpcomingCursor parses the SAME opaque cursor into the ASCENDING
// (scheduledFor, id) anchor used by the upcoming-reservations slice (MYR-360).
func decodeRideUpcomingCursor(s string) (RideRequestUpcomingCursor, error) {
	anchor, id, err := decodeRideCursorAnchor(s)
	if err != nil {
		return RideRequestUpcomingCursor{}, err
	}
	return RideRequestUpcomingCursor{ScheduledFor: anchor, ID: id}, nil
}
