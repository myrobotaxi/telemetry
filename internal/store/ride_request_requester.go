// RideRequestRepo requester-name resolution (MYR-229). The wire RideRequest
// (contracts $defs.RideRequest) and the ride_request_created /
// ride_status_changed WS payloads carry an optional `requesterName` so an
// owner sees who asked for the ride instead of a bare user cuid.
//
// The requester is the ride's rider_id — a Prisma-owned "User" CUID (shared
// DB). Per CG-DL-9 the "User" table is READ-ONLY here; the name/email are read
// INLINE by requesterIdentitySelect (ride_request_queries.go) in the same
// statement as the ride row. This file holds only the PURE fallback chain the
// scanner applies to that inline result — no database access, unit-tested
// without a DB.
//
// Fallback chain (locked in the MYR-229 contract): first name (first
// whitespace-separated token of the display name) → email local-part →
// "Rider". The scanner applies this ONLY when the rider has a "User" row; with
// no row it leaves RequesterName "" and the wire projections omit the field. A
// row that exists but has neither a usable name nor email still resolves to the
// "Rider" literal (never omitted). The resolved value is P1 PII, NEVER logged.

package store

import "strings"

// requesterDisplayName applies the MYR-229 fallback chain to one identity's
// nullable name/email. The caller only invokes it for a rider that HAS a
// "User" row, so a result with neither a usable name token nor an email
// local-part resolves to the "Rider" literal (never ""). Pure — unit-tested
// without a database.
func requesterDisplayName(name, email *string) string {
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
