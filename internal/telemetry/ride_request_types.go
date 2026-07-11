package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
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

// ErrRideStatusConflict is returned by RideRequestStore.UpdateStatusFrom
// when the row exists but its current status is outside the allowed-from
// set — the transition lost a race or was illegal to begin with. The
// handlers map it to HTTP 409 conflict. The cmd adapter translates
// store.ErrRideRequestConflict into this sentinel so the handler layer
// stays decoupled from internal/store.
var ErrRideStatusConflict = errors.New("ride request status conflict")

// ErrRideActive is returned by RideRequestStore.Create when the rider
// already has an OPEN instant ride request and the partial unique guard
// (migration 0004) rejects the second insert. The create handler maps it to
// HTTP 409 `ride_active` and fetches the existing open request (GetActive
// InstantByRider) for the response body. The cmd adapter translates
// store.ErrRideRequestActive into this sentinel so the handler layer stays
// decoupled from internal/store.
var ErrRideActive = errors.New("ride request already active")

// RideRequestStore is the persistence surface the ride-request handlers
// need. Implemented by rideRequestStoreAdapter in cmd/telemetry-server over
// store.RideRequestRepo. MYR-174 uses Create/GetByID/UpdateStatusFrom/
// ListByRiderPage; MYR-175 (owner API) adds ListByOwnerPage.
type RideRequestStore interface {
	// Create inserts a new ride request. Returns ErrRideActive when the
	// rider already holds an OPEN instant ride (the one-active-ride guard,
	// MYR-230) — the partial unique index arbitrates the race, so two
	// concurrent instant creates never both succeed.
	Create(ctx context.Context, in RideRequestCreateInput) (RideRequestData, error)
	GetByID(ctx context.Context, id string) (RideRequestData, error)
	// GetActiveInstantByRider returns the rider's single OPEN instant ride,
	// or an sdk.ErrNotFound-wrapping error when none is open. The create
	// handler uses it to populate the 409 `ride_active` body so the client
	// can adopt the existing ride.
	GetActiveInstantByRider(ctx context.Context, riderID string) (RideRequestData, error)
	// UpdateStatusFrom atomically transitions the ride to `to` ONLY when
	// its current status is in `from` (single guarded UPDATE — the
	// MYR-174/175 check-then-write race fix). Misses return
	// ErrRideStatusConflict (row exists, status outside `from`) or an
	// sdk.ErrNotFound-wrapping error (row gone).
	UpdateStatusFrom(ctx context.Context, id string, from []string, to string) (RideRequestData, error)
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

// rideActiveErrorResponse is the 409 `ride_active` body (MYR-230). It carries
// the standard REST error envelope PLUS the rider's existing OPEN instant
// ride under `activeRideRequest` — the same RideRequest shape
// GET /api/ride-requests/{id} returns — so the SDK can adopt it into the
// pending/tracking UI instead of showing a decline or generic failure. The
// nested `error` object stays byte-compatible with every other error
// response (§4.1); `activeRideRequest` is an additive sibling emitted only
// for this code. Coordinates in the adopted ride are P1 and returned only to
// the ride's own rider (a party) — never logged (§4.1 rule 2).
type rideActiveErrorResponse struct {
	Error             wserrors.ErrorEnvelopeBody `json:"error"`
	ActiveRideRequest rideRequestWire            `json:"activeRideRequest"`
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
