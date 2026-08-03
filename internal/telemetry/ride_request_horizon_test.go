package telemetry

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-372: the scheduledFor future-horizon bound.
//
// Before it, `scheduledFor` was validated only for RFC 3339 syntax, so a rider
// could book a pickup for the year 2400 — a row the sweeper re-evaluates every
// 30 seconds forever, against an availability question nobody can answer.

// horizonNow is the fixed instant the validator table below is judged at, so
// the boundary cases are exact rather than approximately-now.
var horizonNow = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// TestValidateCreateBody_ScheduledForHorizon is the boundary truth table,
// exercised directly against the validator at a fixed clock. The HTTP-level
// assertions below prove the same rule reaches the wire with the right shape.
func TestValidateCreateBody_ScheduledForHorizon(t *testing.T) {
	limit := horizonNow.Add(MaxScheduledForHorizon)

	tests := []struct {
		name         string
		scheduledFor *time.Time
		wantRejected bool
	}{
		{name: "no scheduledFor is an instant ride and unbounded"},
		{
			name:         "day 1 is well inside the horizon",
			scheduledFor: ptrTime(horizonNow.AddDate(0, 0, 1)),
		},
		{
			name:         "day 29 is inside the horizon",
			scheduledFor: ptrTime(horizonNow.AddDate(0, 0, 29)),
		},
		{
			// EQUAL IS ALLOWED, the same reading the service-window bound
			// takes: a client that renders "30 days" must not be refused by
			// its own arithmetic.
			name:         "exactly the horizon is allowed",
			scheduledFor: &limit,
		},
		{
			name:         "one second past the horizon is refused",
			scheduledFor: ptrTime(limit.Add(time.Second)),
			wantRejected: true,
		},
		{
			name:         "day 31 is refused",
			scheduledFor: ptrTime(horizonNow.AddDate(0, 0, 31)),
			wantRejected: true,
		},
		{
			name:         "absurdly far out is refused",
			scheduledFor: ptrTime(horizonNow.AddDate(374, 0, 0)),
			wantRejected: true,
		},
		{
			// THE PAST IS DELIBERATELY UNBOUNDED: §7.8's accept-after-due
			// semantics let an owner accept a reservation whose instant has
			// passed, and the sweeper's lateness ceiling resolves those.
			name:         "the past is not bounded here",
			scheduledFor: ptrTime(horizonNow.AddDate(0, 0, -30)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := rideRequestCreateBody{
				VehicleID: fixtureSnapshotRowID,
				Pickup:    &ridePlaceWire{Lat: 37.77, Lng: -122.41, Label: "Pickup"},
				Dropoff:   &ridePlaceWire{Lat: 37.79, Lng: -122.39, Label: "Dropoff"},
			}
			if tt.scheduledFor != nil {
				s := tt.scheduledFor.Format(time.RFC3339)
				body.ScheduledFor = &s
			}

			in, reason := validateCreateBody(body, horizonNow)

			if tt.wantRejected {
				if reason == "" {
					t.Fatalf("scheduledFor %v was accepted; want a 400 reason", tt.scheduledFor)
				}
				if !strings.Contains(reason, "scheduledFor") {
					t.Errorf("reason must name the offending field, got %q", reason)
				}
				return
			}
			if reason != "" {
				t.Fatalf("scheduledFor %v was refused: %q", tt.scheduledFor, reason)
			}
			if tt.scheduledFor != nil && in.ScheduledFor == nil {
				t.Error("an accepted scheduledFor must survive onto the input")
			}
		})
	}
}

// TestRideRequestCreate_HorizonBoundOnTheWire proves the bound is enforced by
// the endpoint, in the existing 400 invalid_request shape — deliberately NOT a
// new error code, matching how the MYR-316 service-window bound refuses.
func TestRideRequestCreate_HorizonBoundOnTheWire(t *testing.T) {
	tests := []struct {
		name         string
		at           time.Time
		wantRejected bool
	}{
		{name: "day 29 is created", at: time.Now().UTC().AddDate(0, 0, 29)},
		{name: "day 31 is refused", at: time.Now().UTC().AddDate(0, 0, 31), wantRejected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
				&fakeRidePublisher{}, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests",
				createBodyScheduled(tt.at), rideAuthOK)

			if !tt.wantRejected {
				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d want 201 (body %s)", rec.Code, rec.Body.String())
				}
				return
			}

			assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
			if store.createCalled {
				t.Error("a refused create still reached the store")
			}
		})
	}
}

// The refusal names the bound in DAYS rather than echoing the rejected
// instant. The rider supplied that instant, so repeating it tells them nothing;
// the number they need is the limit. It carries no P1 value either way.
func TestRideRequestCreate_HorizonMessageNamesTheBound(t *testing.T) {
	h := newRideHandler(&fakeRideStore{}, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)},
		&fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests",
		createBodyScheduled(time.Now().UTC().AddDate(0, 0, 31)), rideAuthOK)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "30 days") {
		t.Fatalf("message did not name the 30-day bound: %s", rec.Body.String())
	}
}

// TestMaxScheduledForHorizon_IsThirtyDays pins the constant itself. It is the
// one place the number lives — no client, contract enum, migration or SQL
// literal restates it — so a change here is a deliberate product decision that
// must also move the §7.8 sentence quoting it.
func TestMaxScheduledForHorizon_IsThirtyDays(t *testing.T) {
	if want := 30 * 24 * time.Hour; MaxScheduledForHorizon != want {
		t.Fatalf("MaxScheduledForHorizon = %v, want %v", MaxScheduledForHorizon, want)
	}
}
