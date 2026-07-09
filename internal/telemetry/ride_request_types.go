package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Ride-request HTTP surface types (P10 ride-hailing, MYR-174). The wire
// source of truth is contracts schemas/ride-request.schema.json; these Go
// shapes hand-roll the same field names/enums byte-for-byte (conformance CI
// checks this). Handler-local domain types + a cmd-side adapter keep the
// handler decoupled from internal/store (same pattern as the drives handler).

// RidePlaceData is a pickup or drop-off point in the handler layer. Address
// is nil when the place carried a label only (pin-drops, "Current location").
type RidePlaceData struct {
	Latitude  float64
	Longitude float64
	Label     string
	Address   *string
}

// RideRequestCreateInput is what the handler hands the store on create. The
// rider/owner ids are server-derived (rider = JWT sub; owner = the vehicle's
// owner), never client-supplied.
type RideRequestCreateInput struct {
	RiderID        string
	OwnerID        string
	VehicleID      string
	Pickup         RidePlaceData
	Dropoff        RidePlaceData
	PassengerName  *string
	PassengerPhone *string
	ScheduledFor   *time.Time
}

// RideRequestData is the full ride-request aggregate the store returns and
// the handler projects onto the wire RideRequest object.
type RideRequestData struct {
	ID                    string
	RiderID               string
	OwnerID               string
	VehicleID             string
	Pickup                RidePlaceData
	Dropoff               RidePlaceData
	Status                string
	PassengerName         *string
	PassengerPhone        *string
	ScheduledFor          *time.Time
	RescheduleProposedFor *time.Time
	RescheduleStatus      *string
	AcceptedAt            *time.Time
	CompletedAt           *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// RideRequestListCursor is the (createdAt, id) anchor the store resumes a
// keyset scan from. Zero value = first page.
type RideRequestListCursor struct {
	CreatedAt time.Time
	ID        string
}

// RideRequestListPage is one page of a keyset scan plus the has-more probe
// result.
type RideRequestListPage struct {
	Items   []RideRequestData
	HasMore bool
}

// RideRequestStore is the persistence surface the ride-request handlers
// need. Implemented by rideRequestStoreAdapter in cmd/telemetry-server over
// store.RideRequestRepo. MYR-174 uses Create/GetByID/UpdateStatus/
// ListByRiderPage; MYR-175 (owner API) adds ListByOwnerPage.
type RideRequestStore interface {
	Create(ctx context.Context, in RideRequestCreateInput) (RideRequestData, error)
	GetByID(ctx context.Context, id string) (RideRequestData, error)
	UpdateStatus(ctx context.Context, id, status string) (RideRequestData, error)
	ListByRiderPage(ctx context.Context, riderID string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error)
	ListByOwnerPage(ctx context.Context, ownerID string, status *string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error)
}

// RideEventPublisher publishes the ride-hailing domain events onto the event
// bus. events.Bus satisfies it. The WS broadcaster turns the created/
// status-changed events into summary frames unicast to the two parties; the
// accept dispatch event (MYR-175) is the seam MYR-176 subscribes to. A
// publish failure is logged and swallowed by callers — the DB mutation has
// already committed, so a dropped notification must not fail the request
// (clients reconcile via the REST one-shot per FR-9.1/FR-9.2).
type RideEventPublisher interface {
	Publish(ctx context.Context, event events.Event) error
}

// ride-request lifecycle status constants (mirror the contracts
// RideRequestStatus enum, ride-request.schema.json $defs.RideRequestStatus).
const (
	rideStatusRequested = "requested"
	rideStatusAccepted  = "accepted"
	rideStatusDeclined  = "declined"
	rideStatusEnroute   = "enroute"
	rideStatusArrived   = "arrived"
	rideStatusCompleted = "completed"
	rideStatusCancelled = "cancelled"
)

// Ride-request list pagination bounds (rest-api.md §4.2.1, same envelope as
// the drives list).
const (
	rideListDefaultLimit = 20
	rideListMaxLimit     = 100
	rideListMinLimit     = 1
)

// ridePlaceWire is the wire RidePlace ($defs.RidePlace): lat/lng/label
// required, address omitted when absent.
type ridePlaceWire struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Label   string  `json:"label"`
	Address *string `json:"address,omitempty"`
}

// rideRequestWire is the wire RideRequest object. Optional keys are omitted
// (omitempty) when their source pointer is nil, matching the schema's
// omit-when-absent convention.
type rideRequestWire struct {
	ID                    string        `json:"id"`
	RiderID               string        `json:"riderId"`
	OwnerID               string        `json:"ownerId"`
	VehicleID             string        `json:"vehicleId"`
	Pickup                ridePlaceWire `json:"pickup"`
	Dropoff               ridePlaceWire `json:"dropoff"`
	Status                string        `json:"status"`
	PassengerName         *string       `json:"passengerName,omitempty"`
	PassengerPhone        *string       `json:"passengerPhone,omitempty"`
	ScheduledFor          *string       `json:"scheduledFor,omitempty"`
	RescheduleProposedFor *string       `json:"rescheduleProposedFor,omitempty"`
	RescheduleStatus      *string       `json:"rescheduleStatus,omitempty"`
	AcceptedAt            *string       `json:"acceptedAt,omitempty"`
	CompletedAt           *string       `json:"completedAt,omitempty"`
	CreatedAt             string        `json:"createdAt"`
	UpdatedAt             string        `json:"updatedAt"`
}

// rideRequestsPageResponse is the RideRequestsListResponse envelope. Mirrors
// drivesPageResponse: items always present (empty array, never null),
// nextCursor null on the final page.
type rideRequestsPageResponse struct {
	Items      []rideRequestWire `json:"items"`
	NextCursor *string           `json:"nextCursor"`
	HasMore    bool              `json:"hasMore"`
}

// rideRequestCreateBody is the POST /api/ride-requests request body
// (RideRequestCreateRequest). Pointers on pickup/dropoff so a missing key is
// distinguishable from a zero-value place for validation.
type rideRequestCreateBody struct {
	VehicleID      string         `json:"vehicleId"`
	Pickup         *ridePlaceWire `json:"pickup"`
	Dropoff        *ridePlaceWire `json:"dropoff"`
	PassengerName  *string        `json:"passengerName"`
	PassengerPhone *string        `json:"passengerPhone"`
	ScheduledFor   *string        `json:"scheduledFor"`
}

// rideRequestCursor is the opaque base64(JSON) list cursor. Encodes
// (createdAt, id) so pagination is stable across concurrent inserts. createdAt
// travels as RFC3339Nano so it round-trips into the store's timestamptz keyset
// comparison without precision loss.
type rideRequestCursor struct {
	CreatedAt string `json:"createdAt"`
	ID        string `json:"id"`
}

// errMalformedRideCursor is the sentinel every recoverable cursor parse
// failure maps to; the handler surfaces it as 400 invalid_request without
// echoing the internal parse error.
var errMalformedRideCursor = errors.New("malformed ride-request cursor")

// encodeRideCursor serialises a (createdAt, id) anchor into the opaque wire
// cursor. Marshaling two strings cannot fail.
func encodeRideCursor(createdAt time.Time, id string) string {
	raw, _ := json.Marshal(rideRequestCursor{
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		ID:        id,
	})
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeRideCursor parses an opaque cursor into a store-ready anchor. Returns
// errMalformedRideCursor on any bad base64 / JSON / field.
func decodeRideCursor(s string) (RideRequestListCursor, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return RideRequestListCursor{}, errMalformedRideCursor
	}
	var c rideRequestCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return RideRequestListCursor{}, errMalformedRideCursor
	}
	if c.CreatedAt == "" || c.ID == "" {
		return RideRequestListCursor{}, errMalformedRideCursor
	}
	ts, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
	if err != nil {
		return RideRequestListCursor{}, errMalformedRideCursor
	}
	return RideRequestListCursor{CreatedAt: ts, ID: c.ID}, nil
}
