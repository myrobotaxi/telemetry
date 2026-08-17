package telemetry

import (
	"errors"
	"net/http"
	"slices"
	"testing"
)

// Step 8b of the §7.6 deletion sequence: the display-name confirmation row
// (MYR-583). The fakes and helpers live in account_deletion_handler_test.go;
// this file is separate only because that one is already the longest test in
// the package.
//
// WHAT IS ACTUALLY WORTH ASSERTING HERE, given the step is a one-line DELETE:
// that it RUNS at all (a step wired into the interface but never called is the
// realistic regression, and the compiler cannot catch it), that it runs BEFORE
// the identity delete, that its count reaches the audit tally, that a re-run is
// a clean no-op, and that a failure FAILS the deletion rather than being
// swallowed. The last one is the interesting choice: the drive COUNT is
// deliberately non-fatal because a missing statistic must never block erasure,
// but this is a row that must actually go, so it is fatal like every other
// destructive step.
func TestAccountDeletion_NameConfirmationStep(t *testing.T) {
	boom := errors.New("confirmation delete failed")

	tests := []struct {
		name string
		// rows is what the fake reports the DELETE affected.
		rows int
		// err makes the step fail.
		err error

		wantStatus int
		// wantCount is the tally the audit row should carry. Only meaningful
		// when the sequence reached the identity transaction.
		wantCount int
		// wantReached asserts the step was invoked.
		wantReached bool
	}{
		{
			name:        "a confirmed account's row is deleted and counted",
			rows:        1,
			wantStatus:  http.StatusNoContent,
			wantCount:   1,
			wantReached: true,
		},
		{
			name: "an account that never confirmed deletes nothing and still succeeds — " +
				"the absent row IS the unconfirmed state, so there is nothing to fail on",
			rows:        0,
			wantStatus:  http.StatusNoContent,
			wantCount:   0,
			wantReached: true,
		},
		{
			name: "a failing delete fails the whole deletion — the row must actually go, " +
				"and a re-run resumes from here",
			rows:        1,
			err:         boom,
			wantStatus:  http.StatusInternalServerError,
			wantReached: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			data := &fakeAccountData{
				confirmationsDeleted: tc.rows,
				confirmationsErr:     tc.err,
				order:                &order,
			}
			teardown := newFakeAccountTeardown()
			teardown.order = &order

			h := newDeletionHandler(t, AccountDeletionDeps{
				Vehicles: &fakeOwnedVehicleLister{},
				Teardown: teardown,
				Rides:    &fakeRideCanceller{},
				Data:     data,
			})

			if got := callDelete(h).Code; got != tc.wantStatus {
				t.Fatalf("status = %d, want %d (sequence = %v)", got, tc.wantStatus, order)
			}

			at := slices.Index(order, "delete_profile_name_confirmation")
			if tc.wantReached && at < 0 {
				t.Fatalf("the confirmation step never ran; sequence = %v", order)
			}
			if tc.err != nil {
				// A failed step must NOT have let the identity transaction run:
				// the caller's own token resolves through those rows and is what
				// authenticates the re-run that finishes the job.
				if slices.Contains(order, "delete_identity") {
					t.Fatalf("identity was deleted after a failed step; sequence = %v", order)
				}
				return
			}

			identityAt := slices.Index(order, "delete_identity")
			if at > identityAt {
				t.Fatalf("the confirmation must go BEFORE the identity rows; sequence = %v", order)
			}
			if got := data.identityCounts.ProfileNameConfirmationsDeleted; got != tc.wantCount {
				t.Fatalf("audit tally profileNameConfirmationsDeleted = %d, want %d", got, tc.wantCount)
			}

			// Idempotent: the re-run deletes zero and is still a 204, which is
			// what keeps the whole sequence re-runnable after a mid-failure.
			if got := callDelete(h).Code; got != http.StatusNoContent {
				t.Fatalf("re-run status = %d, want 204", got)
			}
			if got := data.identityCounts.ProfileNameConfirmationsDeleted; got != 0 {
				t.Fatalf("re-run tally profileNameConfirmationsDeleted = %d, want 0", got)
			}
		})
	}
}
