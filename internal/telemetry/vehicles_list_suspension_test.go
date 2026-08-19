package telemetry

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// MYR-592 — `VehicleSummary.telemetrySuspendedAt` on §7.0.
//
// The arms this file owns are the wire ones: the key is ALWAYS present, a
// suspension instant reaches it as RFC 3339 UTC, an unsuspended car is an
// explicit null, and BOTH role masks carry it. The DECISION to suspend belongs
// to internal/fleetsuspend and the storage to internal/store; this is the proof
// that whatever they recorded survives the projection and the mask unmangled,
// including the nil.

// catalogRowSuspendedAt is the owner-side catalog row with the suspension stamp
// set (or cleared, for a nil argument).
func catalogRowSuspendedAt(at *time.Time) VehicleCatalogRow {
	row := sharedCatalogRow(true).VehicleCatalogRow
	row.TelemetrySuspendedAt = at
	return row
}

func TestVehicleSummary_TelemetrySuspendedAtArms(t *testing.T) {
	// Deliberately NOT UTC: the projection must normalise, because two clients
	// comparing the same instant across two offsets is a bug nobody reports.
	suspended := time.Date(2026, 8, 19, 5, 0, 0, 0, time.FixedZone("PDT", -7*3600))

	tests := []struct {
		name   string
		at     *time.Time
		want   any
		reason string
	}{
		{
			name:   "a suspended car carries the instant as RFC 3339 UTC",
			at:     &suspended,
			want:   "2026-08-19T12:00:00Z",
			reason: "the same formatting the sibling serviceEstimatedEndAt uses — one rule for both nullable instants on this row",
		},
		{
			name: "a streaming car is an explicit null, not a missing key",
			at:   nil,
			want: nil,
			reason: "an ABSENT key on this contract means 'a server predating v0.38.0', which a client " +
				"is entitled to ignore; null means 'streaming normally', which it must render",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := catalogRowSuspendedAt(tc.at)
			summary := newVehicleSummary(&row, auth.RoleOwner, auth.ShareGrant{}, time.Now())

			m := summary.toMaskMap()
			got, present := m["telemetrySuspendedAt"]
			if !present {
				t.Fatalf("telemetrySuspendedAt must ALWAYS be present in the mask map (%s)", tc.reason)
			}
			if tc.want == nil {
				if got != nil {
					t.Errorf("telemetrySuspendedAt = %v (%T), want an untyped nil (%s)", got, got, tc.reason)
				}
				return
			}
			if got != tc.want {
				t.Errorf("telemetrySuspendedAt = %v (%T), want %v (%s)", got, got, tc.want, tc.reason)
			}
		})
	}
}

// TestVehicleSummary_TelemetrySuspendedAtSurvivesBothMasks is the RBAC arm, and
// the VIEWER case is the one that had to be argued: a viewer who is not told
// renders a permanent spinner over a car that will never stream again, which the
// contract forbids outright.
func TestVehicleSummary_TelemetrySuspendedAtSurvivesBothMasks(t *testing.T) {
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		role  auth.Role
		grant auth.ShareGrant
	}{
		{auth.RoleOwner, auth.ShareGrant{}},
		{auth.RoleViewer, auth.ShareGrant{AllowRides: true}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			row := catalogRowSuspendedAt(&at)
			summary := newVehicleSummary(&row, tc.role, tc.grant, time.Now())
			projected, _ := mask.Apply(summary.toMaskMap(), mask.For(mask.ResourceVehicleSummary, tc.role))

			got, ok := projected["telemetrySuspendedAt"]
			if !ok {
				t.Fatalf("the %s mask dropped telemetrySuspendedAt — it is in both allow-lists", tc.role)
			}
			if got != "2026-08-19T12:00:00Z" {
				t.Errorf("telemetrySuspendedAt = %v, want 2026-08-19T12:00:00Z", got)
			}
		})
	}
}

// TestViewerSummaryMap_CarriesTelemetrySuspendedAt pins the field on the ACTUAL
// viewer projection — the one function both the §7.0 viewer merge and the redeem
// response go through, so this covers both surfaces at once.
func TestViewerSummaryMap_CarriesTelemetrySuspendedAt(t *testing.T) {
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	projected := viewerSummaryMap(catalogRowSuspendedAt(&at), auth.ShareGrant{AllowRides: true}, time.Now())
	got, ok := projected["telemetrySuspendedAt"]
	if !ok {
		t.Fatal("the viewer projection omits telemetrySuspendedAt — a viewer who is not told " +
			"cannot render the honest no-live-telemetry state the contract requires")
	}
	if got != "2026-08-19T12:00:00Z" {
		t.Errorf("telemetrySuspendedAt = %v, want 2026-08-19T12:00:00Z", got)
	}

	// And a streaming viewer row keeps the key, as an explicit null.
	streaming := viewerSummaryMap(catalogRowSuspendedAt(nil), auth.ShareGrant{AllowRides: true}, time.Now())
	v, ok := streaming["telemetrySuspendedAt"]
	if !ok {
		t.Fatal("a streaming viewer row must still carry the key")
	}
	if v != nil {
		t.Errorf("streaming viewer row telemetrySuspendedAt = %v, want nil", v)
	}
}
