package telemetry

import (
	"context"
	"time"
)

// DriveDetailData is the handler-layer view of a single completed
// drive's full FR-3.4 record, decoupled from internal/store (the cmd
// adapter maps a store.DriveRecord into it). It carries every
// DriveDetail wire field EXCEPT routePoints — the heavy polyline is
// served by GET /api/drives/{driveId}/route (rest-api.md §7.4), so the
// detail endpoint never reads or decrypts it.
//
// DurationMinutes is the Prisma-stored unit; buildDriveDetail converts
// it to the durationSeconds wire field per rest-api.md §7.3.
type DriveDetailData struct {
	ID               string
	VehicleID        string
	StartTime        string // ISO 8601 — Prisma string column
	EndTime          string
	Date             string // "2026-04-13" — Prisma string column
	DistanceMiles    float64
	DurationMinutes  int
	AvgSpeedMph      float64
	MaxSpeedMph      float64
	EnergyUsedKwh    float64
	StartChargeLevel int
	EndChargeLevel   int
	FsdMiles         float64
	FsdPercentage    float64
	Interventions    int
	StartLocation    string // P1 — reverse-geocoded place name
	StartAddress     string // P1 — reverse-geocoded street address
	EndLocation      string // P1 — empty until end-side geocoding lands
	EndAddress       string // P1 — empty until end-side geocoding lands
	CreatedAt        time.Time
}

// DriveDetailFetcher fetches a single drive's full record by drive id.
// Implementations MUST return an error wrapping sdk.ErrNotFound when the
// driveId is unknown (the handler maps that to 404). store.ErrDriveNotFound
// already wraps sdk.ErrNotFound, so the adapter only needs to pass it
// through with %w.
type DriveDetailFetcher interface {
	GetDriveDetail(ctx context.Context, driveID string) (DriveDetailData, error)
}

// driveDetail is the DriveDetail wire shape per rest-api.md §7.3 / §8.
// JSON tags mirror the OpenAPI `DriveDetail` component and the per-role
// mask allow-list in `internal/mask/tables.go` (driveDetailFields).
// durationSeconds is computed from DurationMinutes — the Prisma schema
// stores minutes but the contract emits seconds.
//
// Start/End Location + Address are P1 and emitted only when non-empty
// (rest-api.md §7.2 nullable-field convention, shared with the drives
// list). They are optional in the OpenAPI schema; the always-present
// FR-3.4 stats are not.
type driveDetail struct {
	ID               string  `json:"id"`
	VehicleID        string  `json:"vehicleId"`
	StartTime        string  `json:"startTime"`
	EndTime          string  `json:"endTime"`
	Date             string  `json:"date"`
	DistanceMiles    float64 `json:"distanceMiles"`
	DurationSeconds  int     `json:"durationSeconds"`
	AvgSpeedMph      float64 `json:"avgSpeedMph"`
	MaxSpeedMph      float64 `json:"maxSpeedMph"`
	EnergyUsedKwh    float64 `json:"energyUsedKwh"`
	StartChargeLevel int     `json:"startChargeLevel"`
	EndChargeLevel   int     `json:"endChargeLevel"`
	FsdMiles         float64 `json:"fsdMiles"`
	FsdPercentage    float64 `json:"fsdPercentage"`
	Interventions    int     `json:"interventions"`
	StartLocation    string  `json:"startLocation,omitempty"`
	StartAddress     string  `json:"startAddress,omitempty"`
	EndLocation      string  `json:"endLocation,omitempty"`
	EndAddress       string  `json:"endAddress,omitempty"`
	CreatedAt        string  `json:"createdAt"`
}

// toMaskMap returns the detail as a wire-name-keyed map suitable for
// projection through the role-based mask. Owners and viewers see the
// same field set per rest-api.md §5.2.3 — the projection is the
// identity in v1 — but the plumbing is wired now for FR-5.5 readiness.
//
// Empty location / address values are omitted entirely (no key emitted)
// rather than serialized as `""`, matching the drives-list handler and
// the §7.2 nullable-field convention. The always-present FR-3.4 stats
// are emitted unconditionally.
func (d driveDetail) toMaskMap() map[string]any {
	m := map[string]any{
		"id":               d.ID,
		"vehicleId":        d.VehicleID,
		"startTime":        d.StartTime,
		"endTime":          d.EndTime,
		"date":             d.Date,
		"distanceMiles":    d.DistanceMiles,
		"durationSeconds":  d.DurationSeconds,
		"avgSpeedMph":      d.AvgSpeedMph,
		"maxSpeedMph":      d.MaxSpeedMph,
		"energyUsedKwh":    d.EnergyUsedKwh,
		"startChargeLevel": d.StartChargeLevel,
		"endChargeLevel":   d.EndChargeLevel,
		"fsdMiles":         d.FsdMiles,
		"fsdPercentage":    d.FsdPercentage,
		"interventions":    d.Interventions,
		"createdAt":        d.CreatedAt,
	}
	if d.StartLocation != "" {
		m["startLocation"] = d.StartLocation
	}
	if d.StartAddress != "" {
		m["startAddress"] = d.StartAddress
	}
	if d.EndLocation != "" {
		m["endLocation"] = d.EndLocation
	}
	if d.EndAddress != "" {
		m["endAddress"] = d.EndAddress
	}
	return m
}

// buildDriveDetail maps the handler-layer data into the wire shape.
// DurationMinutes is converted to durationSeconds per the §7.3 example;
// CreatedAt is rendered RFC3339 in UTC.
func buildDriveDetail(d DriveDetailData) driveDetail {
	return driveDetail{
		ID:               d.ID,
		VehicleID:        d.VehicleID,
		StartTime:        d.StartTime,
		EndTime:          d.EndTime,
		Date:             d.Date,
		DistanceMiles:    d.DistanceMiles,
		DurationSeconds:  d.DurationMinutes * 60,
		AvgSpeedMph:      d.AvgSpeedMph,
		MaxSpeedMph:      d.MaxSpeedMph,
		EnergyUsedKwh:    d.EnergyUsedKwh,
		StartChargeLevel: d.StartChargeLevel,
		EndChargeLevel:   d.EndChargeLevel,
		FsdMiles:         d.FsdMiles,
		FsdPercentage:    d.FsdPercentage,
		Interventions:    d.Interventions,
		StartLocation:    d.StartLocation,
		StartAddress:     d.StartAddress,
		EndLocation:      d.EndLocation,
		EndAddress:       d.EndAddress,
		CreatedAt:        d.CreatedAt.UTC().Format(time.RFC3339),
	}
}
