package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Tests for the MYR-360 `upcomingForVehicle` slice of the owner incoming feed
// (`GET /api/ride-requests/incoming`). The slice answers exactly one question
// the default feed cannot: "which ACCEPTED FUTURE reservations would an owner
// break by pausing ride sharing on THIS car?" — the default feed is hardcoded
// to `requested` and has no vehicle filter, so decided rows leave it by
// construction (rest-api.md §7.8).

// upcomingRideIDA / upcomingRideIDB are ordered ids for the two reservation
// fixtures; the second sorts after the first so an (scheduledFor, id) tie-break
// assertion has a deterministic answer.
const (
	upcomingRideIDA = "crraaa0123456789abcdef0123456789"
	upcomingRideIDB = "crrbbb0123456789abcdef0123456789"
)

// upcomingRide builds an ACCEPTED, SCHEDULED ride on the fixture vehicle owned
// by `owner` and due at `at` — the shape the slice is meant to return.
func upcomingRide(owner, id string, at time.Time) RideRequestData {
	rec := fixtureRideData(owner, rideStatusAccepted)
	rec.ID = id
	utc := at.UTC()
	rec.ScheduledFor = &utc
	return rec
}

// TestRideRequestHandler_IncomingUpcomingForVehicle pins the slice's routing,
// owner scoping, envelope, and the ASCENDING (scheduledFor, id) cursor.
func TestRideRequestHandler_IncomingUpcomingForVehicle(t *testing.T) {
	const owner = rideOtherUsr
	soon := time.Date(2026, 8, 1, 17, 0, 0, 0, time.UTC)
	later := time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)

	newStore := func() *fakeRideStore {
		return &fakeRideStore{
			// Seeded so a handler that ignored the param and served the DEFAULT
			// feed would return the wrong rows loudly rather than an empty page.
			ownerPage: RideRequestListPage{Items: []RideRequestData{fixtureRideData(owner, rideStatusRequested)}},
			upcomingPage: RideRequestListPage{Items: []RideRequestData{
				upcomingRide(owner, upcomingRideIDA, soon),
				upcomingRide(owner, upcomingRideIDB, later),
			}},
		}
	}

	t.Run("serves the upcoming slice, never the requested feed", func(t *testing.T) {
		st := newStore()
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle="+rideVehicle, "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}

		if st.upcomingCalls != 1 {
			t.Fatalf("ListUpcomingByOwnerVehiclePage calls: got %d want 1", st.upcomingCalls)
		}
		// Owner scoping comes from the JWT sub, never from the query string —
		// no cross-owner read is expressible through this param.
		if st.upcomingCall.ownerID != owner {
			t.Errorf("ownerID: got %q want %q", st.upcomingCall.ownerID, owner)
		}
		if st.upcomingCall.vehicleID != rideVehicle {
			t.Errorf("vehicleID: got %q want %q", st.upcomingCall.vehicleID, rideVehicle)
		}
		if st.upcomingCall.limit != rideListDefaultLimit {
			t.Errorf("limit: got %d want %d", st.upcomingCall.limit, rideListDefaultLimit)
		}
		if st.ownerCall.id != "" {
			t.Errorf("the default requested feed must not be queried, got ownerCall=%+v", st.ownerCall)
		}

		items := decodeUpcomingItems(t, rec.Body.Bytes())
		if len(items) != 2 {
			t.Fatalf("items: got %d want 2 (%s)", len(items), rec.Body.String())
		}
		// Store order is preserved verbatim — the SOONEST-first ordering is the
		// query's job and must not be re-sorted on the way out.
		if items[0].ID != upcomingRideIDA || items[1].ID != upcomingRideIDB {
			t.Errorf("order: got %q,%q want %q,%q", items[0].ID, items[1].ID, upcomingRideIDA, upcomingRideIDB)
		}
		for i, it := range items {
			if it.Status != rideStatusAccepted {
				t.Errorf("item %d status: got %q want %q", i, it.Status, rideStatusAccepted)
			}
			if it.ScheduledFor == nil {
				t.Errorf("item %d: scheduledFor must be present on a reservation", i)
			}
		}
	})

	t.Run("resolves requesterName exactly as every other list item does", func(t *testing.T) {
		name := "Maya"
		row := upcomingRide(owner, upcomingRideIDA, soon)
		row.RequesterName = &name
		st := &fakeRideStore{upcomingPage: RideRequestListPage{Items: []RideRequestData{row}}}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle="+rideVehicle, "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"requesterName":"Maya"`) {
			t.Errorf("requesterName missing from the slice's items: %s", rec.Body.String())
		}
	})

	t.Run("unknown or unowned vehicle is an empty page, not 403/404", func(t *testing.T) {
		// The filter runs on the owner's OWN feed, so a vehicle the caller does
		// not own simply matches no rows. Answering 403/404 would turn the param
		// into an existence oracle for other people's cars.
		st := &fakeRideStore{upcomingPage: RideRequestListPage{Items: nil, HasMore: false}}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle=clsomebodyelsescar0000", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d want 200 (no existence oracle). body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"nextCursor":null`) {
			t.Errorf("envelope: %s", body)
		}
		// The vehicle is still passed through untouched — the store, not the
		// handler, decides there are no rows.
		if st.upcomingCall.vehicleID != "clsomebodyelsescar0000" {
			t.Errorf("vehicleID: got %q", st.upcomingCall.vehicleID)
		}
	})

	t.Run("nextCursor anchors on (scheduledFor, id), not (createdAt, id)", func(t *testing.T) {
		last := upcomingRide(owner, upcomingRideIDB, later)
		st := &fakeRideStore{upcomingPage: RideRequestListPage{Items: []RideRequestData{last}, HasMore: true}}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle="+rideVehicle, "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}

		var env struct {
			NextCursor *string `json:"nextCursor"`
			HasMore     bool   `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !env.HasMore || env.NextCursor == nil {
			t.Fatalf("expected hasMore + cursor: %+v", env)
		}
		// Same opaque wire format as every other ride cursor — one encoding on
		// the wire, two typed views of it.
		anchor, err := decodeRideCursor(*env.NextCursor)
		if err != nil {
			t.Fatalf("cursor is not the shared ride-cursor format: %v", err)
		}
		if anchor.ID != last.ID {
			t.Errorf("cursor id: got %q want %q", anchor.ID, last.ID)
		}
		if !anchor.CreatedAt.Equal(later) {
			t.Errorf("cursor anchor: got %v want the reservation instant %v (createdAt is %v)",
				anchor.CreatedAt, later, last.CreatedAt)
		}
	})

	t.Run("resumes from a (scheduledFor, id) cursor", func(t *testing.T) {
		st := newStore()
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		cursor := encodeRideCursor(soon, upcomingRideIDA)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle="+rideVehicle+"&cursor="+cursor+"&limit=5", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !st.upcomingCall.cursorScheduledFor.Equal(soon) {
			t.Errorf("cursor scheduledFor: got %v want %v", st.upcomingCall.cursorScheduledFor, soon)
		}
		if st.upcomingCall.cursorID != upcomingRideIDA {
			t.Errorf("cursor id: got %q want %q", st.upcomingCall.cursorID, upcomingRideIDA)
		}
		if st.upcomingCall.limit != 5 {
			t.Errorf("limit: got %d want 5", st.upcomingCall.limit)
		}
	})

	t.Run("500 on store failure", func(t *testing.T) {
		st := &fakeRideStore{upcomingErr: fmt.Errorf("db down")}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle="+rideVehicle, "", rideAuthOK)
		assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
	})

	t.Run("401 without auth, before the param is even looked at", func(t *testing.T) {
		st := newStore()
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?upcomingForVehicle="+rideVehicle, "", "")
		assertErrEnvelope(t, rec, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed)
		if st.upcomingCalls != 0 {
			t.Errorf("unauthenticated request must not reach the store, got %d calls", st.upcomingCalls)
		}
	})
}

// TestRideRequestHandler_IncomingUpcomingForVehicle_Validation pins the 400s.
// A malformed value is `invalid_request`, exactly like the existing `limit` /
// `cursor` validation — never a 404, which would leak whether the vehicle
// exists.
func TestRideRequestHandler_IncomingUpcomingForVehicle_Validation(t *testing.T) {
	const owner = rideOtherUsr

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{name: "present but empty", query: "?upcomingForVehicle=", wantStatus: http.StatusBadRequest},
		{name: "whitespace only", query: "?upcomingForVehicle=%20%20", wantStatus: http.StatusBadRequest},
		{name: "oversized", query: "?upcomingForVehicle=" + strings.Repeat("v", 200), wantStatus: http.StatusBadRequest},
		{name: "bad limit alongside", query: "?upcomingForVehicle=" + rideVehicle + "&limit=0", wantStatus: http.StatusBadRequest},
		{name: "bad cursor alongside", query: "?upcomingForVehicle=" + rideVehicle + "&cursor=@@", wantStatus: http.StatusBadRequest},
		{name: "well-formed", query: "?upcomingForVehicle=" + rideVehicle, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeRideStore{}
			h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
			rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming"+tt.query, "", rideAuthOK)
			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, wserrors.ErrCodeInvalidRequest)
				if st.upcomingCalls != 0 {
					t.Errorf("rejected request must not reach the store, got %d calls", st.upcomingCalls)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRideRequestHandler_IncomingDefaultFeedUnchanged is the hard requirement:
// WITHOUT the param the endpoint is byte-identical to what it served before
// MYR-360 — same `requested` status filter, same (createdAt, id) cursor, same
// `createdAt DESC` envelope, same bytes.
func TestRideRequestHandler_IncomingDefaultFeedUnchanged(t *testing.T) {
	const owner = rideOtherUsr
	item := fixtureRideData(owner, rideStatusRequested)

	st := &fakeRideStore{
		ownerPage: RideRequestListPage{Items: []RideRequestData{item}, HasMore: false},
		// Seeded so a handler that took the new branch unconditionally would
		// serve these instead and fail loudly.
		upcomingPage: RideRequestListPage{Items: []RideRequestData{
			upcomingRide(owner, upcomingRideIDA, time.Date(2026, 8, 1, 17, 0, 0, 0, time.UTC)),
		}},
	}
	h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
	rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming?limit=7", "", rideAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	want := fmt.Sprintf(`{"items":[{"id":%q,"riderId":%q,"ownerId":%q,"vehicleId":%q,`+
		`"pickup":{"lat":37.7955,"lng":-122.3937,"label":"Home","address":"221 Folsom St, San Francisco"},`+
		`"dropoff":{"lat":37.7766,"lng":-122.3946,"label":"Caltrain"},`+
		`"status":"requested","createdAt":"2026-06-15T16:12:00Z","updatedAt":"2026-06-15T16:12:00Z"}],`+
		`"nextCursor":null,"hasMore":false}`+"\n",
		item.ID, item.RiderID, item.OwnerID, item.VehicleID)
	if rec.Body.String() != want {
		t.Errorf("default feed body drifted.\n got: %s\nwant: %s", rec.Body.String(), want)
	}

	if st.upcomingCalls != 0 {
		t.Errorf("absent param must not take the upcoming branch, got %d calls", st.upcomingCalls)
	}
	if st.ownerCall.status == nil || *st.ownerCall.status != rideStatusRequested {
		t.Errorf("status filter: %v", st.ownerCall.status)
	}
	if st.ownerCall.id != owner || st.ownerCall.limit != 7 {
		t.Errorf("owner call: id=%q limit=%d", st.ownerCall.id, st.ownerCall.limit)
	}
}

// decodeUpcomingItems pulls the fields the reservation dialog needs out of the
// RideRequestsListResponse envelope.
func decodeUpcomingItems(t *testing.T, body []byte) []struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	ScheduledFor *string `json:"scheduledFor"`
} {
	t.Helper()
	var env struct {
		Items []struct {
			ID           string  `json:"id"`
			Status       string  `json:"status"`
			ScheduledFor *string `json:"scheduledFor"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	return env.Items
}
