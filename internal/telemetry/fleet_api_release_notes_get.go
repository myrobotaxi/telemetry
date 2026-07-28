package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// ReleaseNote is one entry of the Fleet API release-notes list
// (GET /api/1/vehicles/{vin}/release_notes), read to learn the vehicle's
// current FSD software designation (MYR-320).
//
// Tesla returns the entries NEWEST FIRST, and the newest entry's TITLE is the
// only place the Fleet API exposes the FSD designation at all — no
// `vehicle_data` field, no telemetry group, no proto carries it. Live-verified
// against the client's own car, which returned "FSD (Supervised) v14.3.5".
//
// Subtitle and Description are decoded but deliberately UNUSED: they are
// marketing prose about the release, and reading them here documents that we
// looked and chose only the title. Keeping them off the wire also keeps the
// payload we handle small and free of anything worth redacting.
//
// Every field is a pointer so "absent" and "empty" stay distinguishable, per the
// same rule the vehicle_data and service_data decodes follow.
type ReleaseNote struct {
	// Title is the release designation, e.g. "FSD (Supervised) v14.3.5". It is
	// passed through VERBATIM — the shape is Tesla's and may change, so nothing
	// downstream parses, reformats or version-compares it.
	Title *string `json:"title"`
	// Subtitle / Description are Tesla's release blurb. Decoded for shape
	// fidelity only; never persisted and never emitted.
	Subtitle    *string `json:"subtitle"`
	Description *string `json:"description"`
}

// releaseNotesResponse mirrors GET /api/1/vehicles/{vin}/release_notes:
// {"response":{"release_notes":[{"title":…,"subtitle":…,"description":…},…]}}.
type releaseNotesResponse struct {
	Response struct {
		ReleaseNotes []ReleaseNote `json:"release_notes"`
	} `json:"response"`
}

// GetReleaseNotes reads a vehicle's release-notes list from the Fleet API
// (MYR-320). Like GetVehicle / GetVehicleData / GetServiceData this is an
// UNSIGNED authenticated read that MUST target the DIRECT Fleet API base URL,
// never the signing tesla-http-proxy, and it NEVER force-wakes the car: an
// asleep or offline vehicle answers 408 (or another non-2xx), which
// doWithRetry does not retry, and the caller treats any error as a non-fatal
// skip.
//
// An EMPTY list is a legitimate, non-error answer — a car Tesla has no notes
// for — and is returned as an empty slice with a nil error so callers do not
// have to distinguish "no notes" from "read failed" by inspecting an error.
// Callers take the FIRST entry's title (newest first) and MUST treat both an
// empty list and an empty/absent title as "no fsdVersion to write".
func (c *FleetAPIClient) GetReleaseNotes(
	ctx context.Context,
	token string,
	vin string,
) ([]ReleaseNote, error) {
	if token == "" {
		return nil, fmt.Errorf("GetReleaseNotes: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("GetReleaseNotes: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/release_notes"

	c.logger.Debug("fetching vehicle release_notes", slog.String("vin", redactVIN(vin)))

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetReleaseNotes(%s): %w", redactVIN(vin), err)
	}

	var result releaseNotesResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Never include respBody: release prose is unbounded free text and has
		// no business in an error string or a log line.
		return nil, fmt.Errorf("GetReleaseNotes(%s): decode response: %w", redactVIN(vin), err)
	}

	return result.Response.ReleaseNotes, nil
}

// newestReleaseTitle returns the FIRST entry's title from a release-notes list —
// Tesla orders them newest first — or "" when the list is empty, the title key
// is absent, or the title is the empty string. The three cases collapse on
// purpose: all three mean "nothing to write", and the caller's only correct
// response to any of them is to leave the stored value alone.
//
// The value is returned VERBATIM. No trimming, no case folding, no `v` prefix
// stripping: the contract fixes fsdVersion as a pass-through free-form string
// whose shape belongs to Tesla.
func newestReleaseTitle(notes []ReleaseNote) string {
	if len(notes) == 0 || notes[0].Title == nil {
		return ""
	}
	return *notes[0].Title
}
