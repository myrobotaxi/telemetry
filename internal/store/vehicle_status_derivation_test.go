package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppendDerivedStatusSet_OnlyOnMotionFrames pins WHEN the MYR-454 status
// fold attaches itself to the UPDATE.
//
// The rule is narrow on purpose: status is re-derived if and only if this
// frame carried a motion column (gear or speed). A climate-only, media-only or
// charge-only frame must leave the column exactly as it found it — otherwise
// every 5s flush from a stationary car would be re-litigating a status nobody
// asked it to touch, and an 'in_service' or 'charging' value written by
// another owner of the shared Prisma table would be stomped at a cadence it
// could not win against.
func TestAppendDerivedStatusSet_OnlyOnMotionFrames(t *testing.T) {
	tests := []struct {
		name       string
		update     VehicleUpdate
		wantStatus bool
	}{
		{
			name:       "gear frame re-derives",
			update:     VehicleUpdate{GearPosition: strPtr("D")},
			wantStatus: true,
		},
		{
			name:       "speed-only frame re-derives",
			update:     VehicleUpdate{Speed: intPtr(31)},
			wantStatus: true,
		},
		{
			name:       "gear and speed together re-derive once",
			update:     VehicleUpdate{GearPosition: strPtr("P"), Speed: intPtr(0)},
			wantStatus: true,
		},
		{
			name:       "speed zero still re-derives",
			update:     VehicleUpdate{Speed: intPtr(0)},
			wantStatus: true,
		},
		{
			name:       "climate-only frame leaves status alone",
			update:     VehicleUpdate{InteriorTemp: intPtr(67)},
			wantStatus: false,
		},
		{
			name:       "charge-only frame leaves status alone",
			update:     VehicleUpdate{ChargeLevel: intPtr(78)},
			wantStatus: false,
		},
		{
			name: "cleared motion columns do not re-derive",
			update: VehicleUpdate{
				GearPosition: strPtr("D"),
				Speed:        intPtr(31),
				ClearFields:  []string{"gearPosition", "speed"},
			},
			wantStatus: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.update.LastUpdated = time.Now()
			query, _, ok := buildTelemetryUpdate("5YJ3E1EA1NF000001", tt.update, nil)
			if !ok {
				t.Fatal("buildTelemetryUpdate returned ok=false")
			}
			gotStatus := strings.Contains(query, `"status" = CASE`)
			if gotStatus != tt.wantStatus {
				t.Fatalf("status clause present = %v, want %v\nquery: %s",
					gotStatus, tt.wantStatus, query)
			}
			// The whole point of folding this in SQL is atomicity: when the
			// clause IS emitted it must ride the same statement as the motion
			// column it derives from, never a second UPDATE.
			if gotStatus && strings.Count(query, "UPDATE") != 1 {
				t.Fatalf("expected exactly one UPDATE statement, got: %s", query)
			}
		})
	}
}

// TestAppendDerivedStatusSet_ArgsMatchPlaceholders guards the hand-rolled
// placeholder arithmetic in the builder. appendDerivedStatusSet advances
// argIdx by two, and every later append (GPS shadows, nav route, labels,
// lastUpdated, the VIN) numbers itself from that value — an off-by-one here
// would not fail to compile, it would silently bind the wrong argument to the
// wrong column at runtime.
func TestAppendDerivedStatusSet_ArgsMatchPlaceholders(t *testing.T) {
	update := VehicleUpdate{
		GearPosition: strPtr("D"),
		Speed:        intPtr(31),
		ChargeLevel:  intPtr(78),
		InteriorTemp: intPtr(67),
		LastUpdated:  time.Now(),
	}

	query, args, ok := buildTelemetryUpdate("5YJ3E1EA1NF000001", update, nil)
	if !ok {
		t.Fatal("buildTelemetryUpdate returned ok=false")
	}

	// Highest $N referenced in the SQL must equal len(args) exactly.
	maxRef := 0
	for i := 1; i <= len(args)+5; i++ {
		if strings.Contains(query, "$"+strconv.Itoa(i)) {
			maxRef = i
		}
	}
	if maxRef != len(args) {
		t.Fatalf("highest placeholder $%d but %d args supplied\nquery: %s",
			maxRef, len(args), query)
	}

	// Every placeholder from $1..$N must actually appear — a gap means an
	// argument is being consumed by nothing.
	for i := 1; i <= len(args); i++ {
		if !strings.Contains(query, "$"+strconv.Itoa(i)) {
			t.Fatalf("placeholder $%d missing from query: %s", i, query)
		}
	}
}
