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

// Tests for the MYR-400 `activeForVehicle` slice of the owner incoming feed
// (`GET /api/ride-requests/incoming`). The slice answers the one question
// neither existing owner surface could: "WHAT RIDE IS THIS CAR SERVING RIGHT
// NOW?" — the default feed is hardcoded to `requested`, and
// `upcomingForVehicle` is accepted AND strictly future, so a live ride fell
// between them and a client that had not performed the accept itself could
// never recover it (rest-api.md §7.8).

// activeRideIDA / activeRideIDB are ordered ids for the fixtures.
const (
	activeRideIDA = "crr400a123456789abcdef0123456789"
	activeRideIDB = "crr400b123456789abcdef0123456789"
)

// activeRide builds a live ride on the fixture vehicle in the given committed
// status. The owner is pinned to the authenticated caller (rideOtherUsr), which
// is the only owner the slice can ever be scoped to.
func activeRide(id, status string, createdAt time.Time) RideRequestData {
	rec := fixtureRideData(rideOtherUsr, status)
	rec.ID = id
	rec.CreatedAt = createdAt
	rec.UpdatedAt = createdAt
	return rec
}

// TestRideRequestHandler_IncomingActiveForVehicle pins routing, owner scoping,
// the envelope, and the DESCENDING (createdAt, id) cursor this slice shares
// with the default feed.
func TestRideRequestHandler_IncomingActiveForVehicle(t *testing.T) {
	const owner = rideOtherUsr
	newer := time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)
	older := time.Date(2026, 8, 1, 17, 0, 0, 0, time.UTC)

	newStore := func() *fakeRideStore {
		return &fakeRideStore{
			// Seeded so a handler that ignored the param and served the DEFAULT
			// feed — or the sibling upcoming slice — would fail loudly rather
			// than return an empty page.
			ownerPage:    RideRequestListPage{Items: []RideRequestData{fixtureRideData(owner, rideStatusRequested)}},
			upcomingPage: RideRequestListPage{Items: []RideRequestData{upcomingRide(upcomingRideIDA, newer)}},
			activeRidesPage: RideRequestListPage{Items: []RideRequestData{
				activeRide(activeRideIDA, rideStatusEnroute, newer),
				activeRide(activeRideIDB, rideStatusAccepted, older),
			}},
		}
	}

	t.Run("serves the active slice, never the requested feed or the upcoming slice", func(t *testing.T) {
		st := newStore()
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle="+rideVehicle, "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}

		if st.activeRidesCalls != 1 {
			t.Fatalf("ListActiveByOwnerVehiclePage calls: got %d want 1", st.activeRidesCalls)
		}
		// Owner scoping comes from the JWT sub, never from the query string —
		// no cross-owner read is expressible through this param.
		if st.activeRidesCall.ownerID != owner {
			t.Errorf("ownerID: got %q want %q", st.activeRidesCall.ownerID, owner)
		}
		if st.activeRidesCall.vehicleID != rideVehicle {
			t.Errorf("vehicleID: got %q want %q", st.activeRidesCall.vehicleID, rideVehicle)
		}
		if st.activeRidesCall.limit != rideListDefaultLimit {
			t.Errorf("limit: got %d want %d", st.activeRidesCall.limit, rideListDefaultLimit)
		}
		if st.ownerCall.id != "" {
			t.Errorf("the default requested feed must not be queried, got ownerCall=%+v", st.ownerCall)
		}
		if st.upcomingCalls != 0 {
			t.Errorf("the sibling upcoming slice must not be queried, got %d calls", st.upcomingCalls)
		}

		items := decodeActiveItems(t, rec.Body.Bytes())
		if len(items) != 2 {
			t.Fatalf("items: got %d want 2 (%s)", len(items), rec.Body.String())
		}
		// Store order is preserved verbatim — ordering is the query's job.
		if items[0].ID != activeRideIDA || items[1].ID != activeRideIDB {
			t.Errorf("order: got %q,%q want %q,%q", items[0].ID, items[1].ID, activeRideIDA, activeRideIDB)
		}
	})

	// The three committed statuses are the ones the per-vehicle
	// one-active-ride index covers, and every one of them must come back: an
	// owner recovering on a second device may be at any point in the handshake.
	t.Run("returns each of the three committed statuses", func(t *testing.T) {
		for _, status := range []string{rideStatusAccepted, rideStatusArrived, rideStatusEnroute} {
			t.Run(status, func(t *testing.T) {
				st := &fakeRideStore{activeRidesPage: RideRequestListPage{
					Items: []RideRequestData{activeRide(activeRideIDA, status, newer)},
				}}
				h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
				rec := doRequest(t, rideMux(h), http.MethodGet,
					"/api/ride-requests/incoming?activeForVehicle="+rideVehicle, "", rideAuthOK)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				items := decodeActiveItems(t, rec.Body.Bytes())
				if len(items) != 1 || items[0].Status != status {
					t.Fatalf("expected one %s ride, got %s", status, rec.Body.String())
				}
			})
		}
	})

	t.Run("empty when the car is serving nothing", func(t *testing.T) {
		st := &fakeRideStore{activeRidesPage: RideRequestListPage{Items: nil, HasMore: false}}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle="+rideVehicle, "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"nextCursor":null`) {
			t.Errorf("envelope: %s", body)
		}
	})

	t.Run("unknown or unowned vehicle is an empty page, not 403/404", func(t *testing.T) {
		// Identical to the upcoming slice's rule: the filter runs on the
		// owner's OWN feed, so a car the caller does not own simply matches no
		// rows. A 403/404 would turn the param into an existence oracle.
		st := &fakeRideStore{activeRidesPage: RideRequestListPage{Items: nil}}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle=clsomebodyelsescar0000", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d want 200 (no existence oracle). body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("envelope: %s", rec.Body.String())
		}
		// The vehicle is still passed through untouched — the store, not the
		// handler, decides there are no rows.
		if st.activeRidesCall.vehicleID != "clsomebodyelsescar0000" {
			t.Errorf("vehicleID: got %q", st.activeRidesCall.vehicleID)
		}
	})

	t.Run("nextCursor anchors on (createdAt, id) like the default feed", func(t *testing.T) {
		last := activeRide(activeRideIDB, rideStatusAccepted, older)
		st := &fakeRideStore{activeRidesPage: RideRequestListPage{Items: []RideRequestData{last}, HasMore: true}}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle="+rideVehicle, "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}

		var env struct {
			NextCursor *string `json:"nextCursor"`
			HasMore    bool    `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !env.HasMore || env.NextCursor == nil {
			t.Fatalf("expected hasMore + cursor: %+v", env)
		}
		anchor, err := decodeRideCursor(*env.NextCursor)
		if err != nil {
			t.Fatalf("cursor is not the shared ride-cursor format: %v", err)
		}
		if anchor.ID != last.ID || !anchor.CreatedAt.Equal(older) {
			t.Errorf("cursor: got (%v, %q) want (%v, %q)", anchor.CreatedAt, anchor.ID, older, last.ID)
		}
	})

	t.Run("resumes from a (createdAt, id) cursor", func(t *testing.T) {
		st := newStore()
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		cursor := encodeRideCursor(newer, activeRideIDA)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle="+rideVehicle+"&cursor="+cursor+"&limit=5", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !st.activeRidesCall.cursorCreatedAt.Equal(newer) {
			t.Errorf("cursor createdAt: got %v want %v", st.activeRidesCall.cursorCreatedAt, newer)
		}
		if st.activeRidesCall.cursorID != activeRideIDA {
			t.Errorf("cursor id: got %q want %q", st.activeRidesCall.cursorID, activeRideIDA)
		}
		if st.activeRidesCall.limit != 5 {
			t.Errorf("limit: got %d want 5", st.activeRidesCall.limit)
		}
	})

	t.Run("500 on store failure", func(t *testing.T) {
		st := &fakeRideStore{activeRidesErr: fmt.Errorf("db down")}
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle="+rideVehicle, "", rideAuthOK)
		assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
	})

	t.Run("401 without auth, before the param is even looked at", func(t *testing.T) {
		st := newStore()
		h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
		rec := doRequest(t, rideMux(h), http.MethodGet,
			"/api/ride-requests/incoming?activeForVehicle="+rideVehicle, "", "")
		assertErrEnvelope(t, rec, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed)
		if st.activeRidesCalls != 0 {
			t.Errorf("unauthenticated request must not reach the store, got %d calls", st.activeRidesCalls)
		}
	})
}

// TestRideRequestHandler_IncomingActiveForVehicle_Validation pins the 400s.
// A malformed value is `invalid_request`, exactly like the sibling
// `upcomingForVehicle` and the shared `limit` / `cursor` validation — never a
// 404, which would leak whether the vehicle exists.
func TestRideRequestHandler_IncomingActiveForVehicle_Validation(t *testing.T) {
	const owner = rideOtherUsr

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{name: "present but empty", query: "?activeForVehicle=", wantStatus: http.StatusBadRequest},
		{name: "whitespace only", query: "?activeForVehicle=%20%20", wantStatus: http.StatusBadRequest},
		{name: "oversized", query: "?activeForVehicle=" + strings.Repeat("v", 200), wantStatus: http.StatusBadRequest},
		{name: "bad limit alongside", query: "?activeForVehicle=" + rideVehicle + "&limit=0", wantStatus: http.StatusBadRequest},
		{name: "bad cursor alongside", query: "?activeForVehicle=" + rideVehicle + "&cursor=@@", wantStatus: http.StatusBadRequest},
		{name: "well-formed", query: "?activeForVehicle=" + rideVehicle, wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeRideStore{}
			h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
			rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming"+tt.query, "", rideAuthOK)
			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, wserrors.ErrCodeInvalidRequest)
				if st.activeRidesCalls != 0 {
					t.Errorf("rejected request must not reach the store, got %d calls", st.activeRidesCalls)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRideRequestHandler_IncomingVehicleFiltersAreExclusive pins that asking
// for both slices at once is REFUSED rather than silently resolved by
// precedence. They are disjoint by construction, so no single page could
// honour both, and answering with one of them would hand a client a
// confidently wrong page.
func TestRideRequestHandler_IncomingVehicleFiltersAreExclusive(t *testing.T) {
	const owner = rideOtherUsr
	st := &fakeRideStore{}
	h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, owner)
	rec := doRequest(t, rideMux(h), http.MethodGet,
		"/api/ride-requests/incoming?activeForVehicle="+rideVehicle+"&upcomingForVehicle="+rideVehicle, "", rideAuthOK)
	assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
	if st.activeRidesCalls != 0 || st.upcomingCalls != 0 {
		t.Errorf("neither slice must be queried: active=%d upcoming=%d", st.activeRidesCalls, st.upcomingCalls)
	}
}

// decodeActiveItems pulls the fields the owner's recovery path needs out of the
// RideRequestsListResponse envelope.
func decodeActiveItems(t *testing.T, body []byte) []struct {
	ID     string `json:"id"`
	Status string `json:"status"`
} {
	t.Helper()
	var env struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	return env.Items
}
