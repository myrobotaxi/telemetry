package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// Drive list pagination defaults per rest-api.md §4.2.1.
const (
	driveListDefaultLimit = 20
	driveListMaxLimit     = 100
	driveListMinLimit     = 1
)

// DriveListItem is the per-row drive shape consumed by the drives
// handler. Mirrors the DriveSummary mask allow-list in
// `internal/mask/tables.go` (driveSummaryFields). The adapter in
// `cmd/telemetry-server` wires `store.DriveRepo.ListByVehicleID` into
// this interface and converts `store.DriveSummaryRow` → DriveListItem
// at the boundary so the handler stays decoupled from `internal/store`.
//
// MYR-145 added Start/End Location + Address. They are reverse-geocoded
// place names + street addresses written by `internal/store/writer_drives.go`
// (drive-start and drive-end handlers) and surfaced verbatim — empty
// strings indicate "not geocoded yet" (e.g., drive still in progress
// for end-side fields, or zero-GPS at drive start). The handler
// translates empty values into omitted JSON keys per rest-api.md §7.2
// nullable-field convention so the SDK can branch on presence.
type DriveListItem struct {
	ID               string
	VehicleID        string
	StartTime        string // ISO 8601 — Prisma string column
	EndTime          string
	Date             string
	StartLocation    string // P1 — reverse-geocoded place name
	StartAddress     string // P1 — reverse-geocoded street address
	EndLocation      string // P1 — empty while drive in progress
	EndAddress       string // P1 — empty while drive in progress
	DistanceMiles    float64
	DurationMinutes  int
	AvgSpeedMph      float64
	MaxSpeedMph      float64
	StartChargeLevel int
	EndChargeLevel   int
	CreatedAt        time.Time
}

// DriveListPage is the result of a single paginated drive read. The
// store-layer adapter populates HasMore based on whether the limit+1
// probe returned an extra row.
type DriveListPage struct {
	Items   []DriveListItem
	HasMore bool
}

// DriveLister returns a page of drives for a vehicle, ordered by
// (startTime DESC, id DESC) per rest-api.md §4.2.2. The cursor encodes
// the (startTime, id) of the last item from the prior page; pass the
// zero-value DriveListCursor on the first call.
type DriveLister interface {
	ListByVehicleID(ctx context.Context, vehicleID string, cursor DriveListCursor, limit int) (DriveListPage, error)
}

// DriveListCursor is the decoded cursor pair the store uses to anchor
// a paginated scan. Zero value means "first page" — the store SHOULD
// treat empty StartTime as "no anchor" rather than match an empty
// string.
type DriveListCursor struct {
	StartTime string `json:"startTime"`
	ID        string `json:"id"`
}

// IsZero reports whether the cursor carries no anchor (first page).
func (c DriveListCursor) IsZero() bool {
	return c.StartTime == "" && c.ID == ""
}

// encodeDriveCursor serializes a (startTime, id) pair to the opaque
// base64-encoded JSON shape exposed in rest-api.md §4.2.1.
func encodeDriveCursor(c DriveListCursor) string {
	raw, _ := json.Marshal(c) // marshaling two strings cannot fail
	return base64.StdEncoding.EncodeToString(raw)
}

// decodeDriveCursor parses an opaque cursor string. Returns
// errMalformedCursor when the input is not valid base64 or not a
// well-formed JSON object — the handler surfaces this as a
// 400 invalid_request per rest-api.md §4.2.1.
func decodeDriveCursor(s string) (DriveListCursor, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return DriveListCursor{}, errMalformedCursor
	}
	var out DriveListCursor
	if err := json.Unmarshal(raw, &out); err != nil {
		return DriveListCursor{}, errMalformedCursor
	}
	if out.StartTime == "" || out.ID == "" {
		return DriveListCursor{}, errMalformedCursor
	}
	return out, nil
}

// errMalformedCursor is returned by decodeDriveCursor on every
// recoverable parse failure. Kept as a sentinel so the handler can
// branch on it without exposing the internal parse error to the wire
// (the response carries the typed wserrors.ErrCodeInvalidRequest only).
var errMalformedCursor = errors.New("malformed cursor")

// driveSummary is the per-row drive shape returned by the endpoint.
// JSON tags mirror the DriveSummary schema in the rest-api.md §7.2
// example and the per-role mask allow-list in
// `internal/mask/tables.go` (driveSummaryFields). The durationSeconds
// wire field is computed from store.DurationMinutes — the Prisma
// schema stores minutes but the contract emits seconds.
//
// MYR-145: Start/End Location + Address are emitted when non-empty.
// `toMaskMap` drops empty values entirely so the wire never carries an
// empty-string place-name (consumers branch on key presence per the
// rest-api.md §7.2 nullable-field convention).
type driveSummary struct {
	ID               string  `json:"id"`
	VehicleID        string  `json:"vehicleId"`
	StartTime        string  `json:"startTime"`
	EndTime          string  `json:"endTime"`
	Date             string  `json:"date"`
	StartLocation    string  `json:"startLocation,omitempty"`
	StartAddress     string  `json:"startAddress,omitempty"`
	EndLocation      string  `json:"endLocation,omitempty"`
	EndAddress       string  `json:"endAddress,omitempty"`
	DistanceMiles    float64 `json:"distanceMiles"`
	DurationSeconds  int     `json:"durationSeconds"`
	AvgSpeedMph      float64 `json:"avgSpeedMph"`
	MaxSpeedMph      float64 `json:"maxSpeedMph"`
	StartChargeLevel int     `json:"startChargeLevel"`
	EndChargeLevel   int     `json:"endChargeLevel"`
	CreatedAt        string  `json:"createdAt"`
}

// toMaskMap returns the summary as a wire-name-keyed map suitable for
// projection through the role-based mask. Owners and viewers see the
// same field set per rest-api.md §5.2.2 — the projection is the
// identity in v1 — but the plumbing is wired now for FR-5.5 readiness.
//
// Empty location / address values are omitted entirely (no key emitted)
// rather than serialized as `""`. The store layer stores
// not-yet-geocoded rows as `""` (Prisma column default), and the SDK
// branches on key presence per rest-api.md §7.2.
func (d driveSummary) toMaskMap() map[string]any {
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
		"startChargeLevel": d.StartChargeLevel,
		"endChargeLevel":   d.EndChargeLevel,
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

// drivesPageResponse is the envelope shape returned by the endpoint
// per rest-api.md §7.2. Matches the PaginatedDrives component in
// specs/rest.openapi.yaml.
type drivesPageResponse struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"nextCursor"`
	HasMore    bool             `json:"hasMore"`
}

// buildDriveSummary maps the store-layer row into the wire shape.
// DurationMinutes is converted to durationSeconds per the §7.2
// example. Empty location/address fields propagate as empty strings —
// `toMaskMap` drops them from the JSON map before encoding.
func buildDriveSummary(row *DriveListItem) driveSummary {
	return driveSummary{
		ID:               row.ID,
		VehicleID:        row.VehicleID,
		StartTime:        row.StartTime,
		EndTime:          row.EndTime,
		Date:             row.Date,
		StartLocation:    row.StartLocation,
		StartAddress:     row.StartAddress,
		EndLocation:      row.EndLocation,
		EndAddress:       row.EndAddress,
		DistanceMiles:    row.DistanceMiles,
		DurationSeconds:  row.DurationMinutes * 60,
		AvgSpeedMph:      row.AvgSpeedMph,
		MaxSpeedMph:      row.MaxSpeedMph,
		StartChargeLevel: row.StartChargeLevel,
		EndChargeLevel:   row.EndChargeLevel,
		CreatedAt:        row.CreatedAt.UTC().Format(time.RFC3339),
	}
}
