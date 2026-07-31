package telemetry

import (
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// The OWNER-FACING ShareInvite serializer (rest-api.md §7.5,
// schemas/vehicle-sharing.schema.json). Split out of share_invite_types.go so
// the projection — which decides which of six conditional keys reach an owner,
// two of them live credentials — can be read on its own.
//
// Every conditional here is contractual, not cosmetic. `code` and `shareUrl`
// exist only while the row is pending; `expiresAt` likewise; `acceptedAt` only
// once accepted; and `allowRides` / `suspended` only on an accepted grant, where
// there is a grant for them to describe.

// shareInviteWire is the owner-facing ShareInvite object.
//
// Four fields are conditionally OMITTED, and the omission is contractual, not
// cosmetic: `code`, `shareUrl` and `expiresAt` exist only while the row is
// pending (an accepted grant's code is not re-readable and an accepted grant
// does not expire), and `acceptedAt` exists only once it has been accepted.
type shareInviteWire struct {
	InviteID  string `json:"inviteId"`
	VehicleID string `json:"vehicleId"`
	Label     string `json:"label"`
	// Permission is the invite-time preset on a pending row and a DERIVED
	// projection of the flags on an accepted one (MYR-369) — never the
	// stored preset for an accepted grant, so a legacy live_history row
	// serializes as `live` and a patched grant serializes what it now
	// conveys.
	Permission string `json:"permission"`
	Status     string `json:"status"`
	// AllowRides / Suspended are the MYR-369 per-grant flags. Pointers so
	// they can be OMITTED on a pending row (where there is no grant) while
	// still serializing an explicit `false` on an accepted one — the
	// contract distinguishes "absent, server predates the field" from
	// "present and false", and a bare bool with omitempty would collapse
	// every false into absence.
	AllowRides *bool  `json:"allowRides,omitempty"`
	Suspended  *bool  `json:"suspended,omitempty"`
	Code       string `json:"code,omitempty"`
	// ShareURL is the signed join link (MYR-368). It CONTAINS the code, so
	// it is P1 and bearer exactly as Code is: never logged, never echoed
	// into an error. Empty when no signing key is configured, in which case
	// the key is omitted and a consumer falls back to sharing Code.
	ShareURL   string `json:"shareUrl,omitempty"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	AcceptedAt string `json:"acceptedAt,omitempty"`
}

// shareInviteListResponse is the GET envelope. The key is `invites`, NOT the
// `items` used by the cursor-paginated list envelopes: this surface is
// deliberately unpaginated and the distinct key keeps an SDK pagination helper
// from mistaking it for a page.
type shareInviteListResponse struct {
	Invites []map[string]any `json:"invites"`
}

// redeemShareInviteResponse is the invited party's 200 body: everything the
// join-success screen needs without a second round trip.
type redeemShareInviteResponse struct {
	OwnerFirstName string           `json:"ownerFirstName"`
	Vehicles       []map[string]any `json:"vehicles"`
}

// toShareInviteWire projects a row onto the wire shape, applying the
// conditional omissions. signer may be nil, in which case shareUrl is omitted.
//
// The code is emitted ONLY for a pending row. The store already blanks it for
// non-pending rows in SQL; this is the second, independent gate, because a
// leaked accepted-grant code is a live credential handed to whoever can list
// the invite. `shareUrl` is minted inside the SAME branch and from the SAME
// two values, so the link cannot outlive the code it embeds — an accepted row
// that somehow carried a URL would leak the credential the code branch just
// withheld.
func toShareInviteWire(row *ShareInviteRow, link inviteLinkCtx) shareInviteWire {
	out := shareInviteWire{
		InviteID:   row.ID,
		VehicleID:  row.VehicleID,
		Label:      row.Label,
		Permission: wirePermission(row),
		Status:     row.Status,
		CreatedAt:  row.CreatedAt.UTC().Format(time.RFC3339),
	}
	// The flags exist only once there is a grant. Emitting them on a pending
	// row would describe capabilities nobody holds yet, and a client reading
	// `suspended: false` there would reasonably conclude the invite is live
	// access rather than an unredeemed code.
	if row.Status == shareStatusAccepted {
		allowRides, suspended := row.Grant.AllowRides, row.Grant.Suspended
		out.AllowRides = &allowRides
		out.Suspended = &suspended
	}
	if row.Status == shareStatusPending {
		out.Code = row.Code
		if !row.ExpiresAt.IsZero() {
			out.ExpiresAt = row.ExpiresAt.UTC().Format(time.RFC3339)
		}
		// Signed over the row's ACTUAL expires_at, so the instant in
		// the link is the instant redemption enforces.
		out.ShareURL = link.signer.ShareURL(row.Code, row.ExpiresAt, link.ownerName, row.Label)
	}
	if row.AcceptedAt != nil {
		out.AcceptedAt = row.AcceptedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// toShareInviteMasked projects a row onto the wire AND through the
// ResourceInvite owner allow-list (internal/mask/tables.go inviteOwnerFields,
// rest-api.md §5.2.5).
//
// Running the mask on a surface that is already owner-gated at the router looks
// redundant, and that is the point: it makes the allow-list LOAD-BEARING. A
// field added to shareInviteWire without a matching entry in the table is
// dropped from the response instead of shipping unclassified — which is exactly
// the drift that left the table describing a Prisma `Invite` shape this server
// never served. The viewer role has no entry for this resource, so if a viewer
// ever reached this code the same call would return an empty object rather than
// an owner's labels and a live code.
func toShareInviteMasked(row *ShareInviteRow, role auth.Role, link inviteLinkCtx) map[string]any { //nolint:unparam // owner is the ONLY role this surface has, and passing it explicitly is what keeps the mask lookup load-bearing rather than an implicit owner projection — see the doc above
	wire := toShareInviteWire(row, link)
	fields := map[string]any{
		"inviteId":   wire.InviteID,
		"vehicleId":  wire.VehicleID,
		"label":      wire.Label,
		"permission": wire.Permission,
		"status":     wire.Status,
		"createdAt":  wire.CreatedAt,
	}
	// The flags are pointer-valued precisely so an explicit `false`
	// survives into the map. Nil means "pending row, no grant" and stays
	// absent.
	if wire.AllowRides != nil {
		fields["allowRides"] = *wire.AllowRides
	}
	if wire.Suspended != nil {
		fields["suspended"] = *wire.Suspended
	}
	// The four conditional keys are added only when they have a value, so
	// omission stays omission rather than becoming an empty string.
	if wire.Code != "" {
		fields["code"] = wire.Code
	}
	if wire.ShareURL != "" {
		fields["shareUrl"] = wire.ShareURL
	}
	if wire.ExpiresAt != "" {
		fields["expiresAt"] = wire.ExpiresAt
	}
	if wire.AcceptedAt != "" {
		fields["acceptedAt"] = wire.AcceptedAt
	}
	projected, _ := mask.Apply(fields, mask.For(mask.ResourceInvite, role))
	return projected
}

// wirePermission produces the `permission` a row serializes.
//
// PENDING rows carry the stored preset — that is what the owner picked and what
// redemption will map onto flags — but NORMALIZED, so a row written before
// MYR-369 at the retired live_history tier does not serialize a value the
// contract says is never emitted.
//
// ACCEPTED rows DERIVE it from the flags and ignore the stored preset entirely.
// That is the whole compatibility story: a pre-MYR-369 client reading
// `permission` on a grant the owner has patched sees the change, and a legacy
// live_history grant reads as `live`, which is exactly what it now conveys.
func wirePermission(row *ShareInviteRow) string {
	if row.Status == shareStatusAccepted {
		return row.Grant.Permission().String()
	}
	return auth.NormalizeInvitePermission(auth.SharePermission(row.Permission)).String()
}
