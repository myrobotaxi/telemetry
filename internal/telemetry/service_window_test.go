package telemetry

import (
	"testing"
	"time"
)

var (
	swTesla = time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	swOwner = time.Date(2026, 8, 3, 9, 30, 0, 0, time.UTC)
)

func ptrTime(t time.Time) *time.Time { return &t }

// TestResolveServiceEstimatedEndAt pins BOTH contract rules in one table: the
// source precedence and the in-service-only gate. This resolver is shared by
// the snapshot, the catalog list and the scheduler bound, so a regression here
// would let the scheduler admit rides the UI had already refused.
func TestResolveServiceEstimatedEndAt(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		tesla    *time.Time
		owner    *time.Time
		want     *time.Time
		wantWire string // "" means JSON null
	}{
		{
			name:     "tesla wins over owner",
			status:   serviceStatusInService,
			tesla:    ptrTime(swTesla),
			owner:    ptrTime(swOwner),
			want:     ptrTime(swTesla),
			wantWire: "2026-08-01T15:00:00Z",
		},
		{
			name:     "owner is the fallback when tesla has nothing",
			status:   serviceStatusInService,
			tesla:    nil,
			owner:    ptrTime(swOwner),
			want:     ptrTime(swOwner),
			wantWire: "2026-08-03T09:30:00Z",
		},
		{
			name:   "neither source yields null — common and normal",
			status: serviceStatusInService,
			want:   nil,
		},
		{
			// The auto-clear rule. Even with both columns populated (the window
			// between the car leaving service and the monitor noticing), a car
			// that is not in service NEVER carries a value.
			name:   "parked vehicle emits null despite populated columns",
			status: "parked",
			tesla:  ptrTime(swTesla),
			owner:  ptrTime(swOwner),
			want:   nil,
		},
		{
			name:   "driving vehicle emits null",
			status: "driving",
			tesla:  ptrTime(swTesla),
			want:   nil,
		},
		{
			name:   "offline vehicle emits null",
			status: "offline",
			owner:  ptrTime(swOwner),
			want:   nil,
		},
		{
			name:   "charging vehicle emits null",
			status: "charging",
			tesla:  ptrTime(swTesla),
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveServiceEstimatedEndAt(tt.status, tt.tesla, tt.owner)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("resolved = %v, want nil", got)
			case tt.want != nil && got == nil:
				t.Fatalf("resolved = nil, want %v", tt.want)
			case tt.want != nil && !got.Equal(*tt.want):
				t.Fatalf("resolved = %v, want %v", got, tt.want)
			}

			wire := serviceEstimatedEndAtWire(tt.status, tt.tesla, tt.owner)
			if tt.wantWire == "" {
				if wire != nil {
					t.Fatalf("wire = %q, want nil (JSON null)", *wire)
				}
				return
			}
			if wire == nil {
				t.Fatalf("wire = nil, want %q", tt.wantWire)
			}
			if *wire != tt.wantWire {
				t.Fatalf("wire = %q, want %q", *wire, tt.wantWire)
			}
		})
	}
}

// A non-UTC instant must still render as a UTC RFC 3339 string — the contract
// specifies a UTC instant, and a service centre's estimate arrives in whatever
// zone Tesla felt like using.
func TestServiceEstimatedEndAtWire_NormalizesToUTC(t *testing.T) {
	zone := time.FixedZone("PDT", -7*3600)
	local := time.Date(2026, 8, 1, 8, 0, 0, 0, zone)

	wire := serviceEstimatedEndAtWire(serviceStatusInService, &local, nil)
	if wire == nil {
		t.Fatal("wire = nil, want a value")
	}
	if *wire != "2026-08-01T15:00:00Z" {
		t.Fatalf("wire = %q, want %q", *wire, "2026-08-01T15:00:00Z")
	}
}
