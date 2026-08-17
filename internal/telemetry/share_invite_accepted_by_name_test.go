package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"
)

// MYR-581 — `ShareInvite.acceptedByName` on the §7.5.2 owner listing.
//
// WHAT THIS FIELD IS FOR, restated because the field it sits beside makes it easy
// to mistake for a duplicate: `label` is the OWNER'S OWN MEMO, typed in the
// send-invite sheet before anybody redeemed anything and never resolved against an
// account. An accepted row therefore said who the owner MEANT to invite, and a
// forwarded code left it naming the wrong person entirely. `acceptedByName` is who
// actually holds the grant.
//
// The three arms below are the whole contract: accepted + resolvable → the name;
// accepted + unresolvable → absent; pending → absent. The last two are
// deliberately the SAME wire value, because `status` already tells them apart and
// a second sentinel would be a state with no copy.

// acceptedInviteRowNamed is an accepted grant whose holder has a resolvable name.
func acceptedInviteRowNamed(first string) ShareInviteRow {
	row := acceptedInviteRow()
	row.AcceptedByName = &first
	return row
}

func TestShareInviteHandler_List_AcceptedByNameArms(t *testing.T) {
	invitePath := "/api/vehicles/" + shareFixtureVeh + "/invites"

	tests := []struct {
		name string
		row  ShareInviteRow
		// want is the expected wire value; nil means the key must be ABSENT.
		want *string
	}{
		{
			name: "an accepted grant carries the redeemer's first name",
			// "Mira" rather than "Mira Chen": the store reduced it, and the
			// reduction is the P1 first-names-only policy — the same policy that
			// governs the owner's name going the other way, to this same person.
			row:  acceptedInviteRowNamed("Mira"),
			want: strptr("Mira"),
		},
		{
			name: "an accepted grant whose account has NO resolvable name omits the key",
			row:  acceptedInviteRow(),
			want: nil,
		},
		{
			name: "a PENDING row omits the key — nobody has accepted it, so there is nobody to name",
			row:  pendingInviteRow(),
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeShareInviteStore{listed: []ShareInviteRow{tc.row}}
			mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

			rec := doShareRequest(t, mux, http.MethodGet, invitePath, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
			}

			var body struct {
				Invites []map[string]any `json:"invites"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Invites) != 1 {
				t.Fatalf("got %d invites, want 1", len(body.Invites))
			}

			got, present := body.Invites[0]["acceptedByName"]
			if tc.want == nil {
				if present {
					t.Errorf("acceptedByName must be ABSENT here, got %v", got)
				}
				return
			}
			if !present {
				t.Fatalf("acceptedByName is missing — the mask allow-list entry is what makes it reachable. row: %v",
					body.Invites[0])
			}
			if got != *tc.want {
				t.Errorf("acceptedByName = %v, want %q", got, *tc.want)
			}
		})
	}
}

// TestShareInviteHandler_List_AcceptedByNameDoesNotReplaceLabel pins the pair.
// Both are P1 names about the same relationship and they are NOT interchangeable:
// the owner's memo stays exactly as typed (nothing overwrites it at redemption),
// and the resolved name arrives beside it. A change that made one shadow the other
// would silently drop information an owner relies on when deciding to revoke.
func TestShareInviteHandler_List_AcceptedByNameDoesNotReplaceLabel(t *testing.T) {
	invitePath := "/api/vehicles/" + shareFixtureVeh + "/invites"
	// The memo and the real holder disagree — the forwarded-code case, which is
	// exactly why the field exists.
	store := &fakeShareInviteStore{listed: []ShareInviteRow{acceptedInviteRowNamed("Sam")}}
	mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

	rec := doShareRequest(t, mux, http.MethodGet, invitePath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Invites []map[string]any `json:"invites"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	row := body.Invites[0]
	if row["label"] != "Mira Chen" {
		t.Errorf("label = %v, want the owner's memo %q left untouched", row["label"], "Mira Chen")
	}
	if row["acceptedByName"] != "Sam" {
		t.Errorf("acceptedByName = %v, want %q — the person who actually redeemed it", row["acceptedByName"], "Sam")
	}
}
