// RideRequestRepo requester-name resolution (MYR-229). The wire RideRequest
// (contracts $defs.RideRequest) and the ride_request_created /
// ride_status_changed WS payloads carry an optional `requesterName` so an
// owner sees who asked for the ride instead of a bare user cuid.
//
// The requester is the ride's rider_id — a Prisma-owned "User" CUID (shared
// DB). Per CG-DL-9 the "User" table is READ-ONLY here: we SELECT name/email,
// never write. The resolved value is P1 PII and is NEVER logged.
//
// Fallback chain (locked in the MYR-229 contract): first name (first
// whitespace-separated token of the display name) → email local-part →
// "Rider". The field is OMITTED (empty string here) ONLY when the rider has
// no "User" row at all; a row that exists but has neither name nor email
// still resolves to the "Rider" literal.

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// queryRequesterIdentity reads one requester's display name + email from the
// Prisma-owned "User" table by CUID (READ-ONLY, CG-DL-9). Both columns are
// nullable in the Prisma schema, so they scan into pointers.
const queryRequesterIdentity = `SELECT "name", "email" FROM "User" WHERE "id" = $1`

// queryRequesterIdentitiesByIDs is the batched variant for list endpoints —
// one round-trip for every distinct rider on the page, so the list projections
// never issue a per-row lookup (no N+1).
const queryRequesterIdentitiesByIDs = `SELECT "id", "name", "email" FROM "User" WHERE "id" = ANY($1)`

// requesterDisplayName applies the MYR-229 fallback chain to one identity.
// found=false (no "User" row for the rider) yields "" so the caller omits the
// field. A found row with neither a usable name token nor an email local-part
// resolves to the "Rider" literal (never omitted). Pure — unit-tested without
// a database.
func requesterDisplayName(found bool, name, email *string) string {
	if !found {
		return ""
	}
	if name != nil {
		if token := firstNameToken(*name); token != "" {
			return token
		}
	}
	if email != nil {
		if local := emailLocalPart(*email); local != "" {
			return local
		}
	}
	return "Rider"
}

// firstNameToken returns the first whitespace-separated token of a display
// name, or "" when the name is empty/all-whitespace. strings.Fields collapses
// any run of Unicode whitespace, so "  Ada  Lovelace " → "Ada".
func firstNameToken(display string) string {
	fields := strings.Fields(display)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// emailLocalPart returns the part before the first '@', or "" when the input
// has no '@' or an empty local-part (e.g. "@example.com"). It does not attempt
// full RFC-5321 validation — the address is only ever a display fallback.
func emailLocalPart(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return ""
	}
	return email[:at]
}

// requesterName resolves a single rider's display name via the fallback chain.
// A missing "User" row returns ("", nil) — the caller omits the field. A query
// failure is propagated (the "User" table shares the pool with go_ride_requests,
// so an outage here means the surrounding read/write is already unhealthy).
func (r *RideRequestRepo) requesterName(ctx context.Context, riderID string) (string, error) {
	var name, email *string
	err := r.pool.QueryRow(ctx, queryRequesterIdentity, riderID).Scan(&name, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve requester name: %w", err)
	}
	return requesterDisplayName(true, name, email), nil
}

// attachRequesterName resolves rec.RiderID's display name and stamps it onto
// the record in place. Kept as a helper so every single-row accessor threads
// the field identically.
func (r *RideRequestRepo) attachRequesterName(ctx context.Context, rec *RideRequestRecord) error {
	name, err := r.requesterName(ctx, rec.RiderID)
	if err != nil {
		return err
	}
	rec.RequesterName = name
	return nil
}

// attachRequesterNames stamps RequesterName onto every record on a list page
// with a single batched "User" lookup keyed on the distinct rider ids — no
// per-row query (avoids N+1). Riders with no "User" row are simply absent from
// the map, leaving RequesterName "" (omitted on the wire).
func (r *RideRequestRepo) attachRequesterNames(ctx context.Context, recs []RideRequestRecord) error {
	if len(recs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(recs))
	ids := make([]string, 0, len(recs))
	for i := range recs {
		id := recs[i].RiderID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	rows, err := r.pool.Query(ctx, queryRequesterIdentitiesByIDs, ids)
	if err != nil {
		return fmt.Errorf("resolve requester names: %w", err)
	}
	defer rows.Close()

	names := make(map[string]string, len(ids))
	for rows.Next() {
		var id string
		var name, email *string
		if scanErr := rows.Scan(&id, &name, &email); scanErr != nil {
			return fmt.Errorf("resolve requester names: scan: %w", scanErr)
		}
		names[id] = requesterDisplayName(true, name, email)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resolve requester names: rows: %w", err)
	}

	for i := range recs {
		recs[i].RequesterName = names[recs[i].RiderID]
	}
	return nil
}
