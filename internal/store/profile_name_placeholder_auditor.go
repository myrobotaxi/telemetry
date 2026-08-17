package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditActionProfileNamePlaceholderScrubbed records the ONE-TIME MYR-583
// operator clearing of the legacy web onboarding's literal 'Tesla User'
// placeholder out of `"User"."name"`.
//
// It is its OWN action rather than a reuse of anything existing, because none of
// the existing actions describes it: it is not a deletion, not a pruning pass,
// not an operator READ, and not a rename the user performed. The question it has
// to answer years later is "why did this account's name column change without the
// account touching it?", and only a dedicated action answers that in one query.
//
// data-lifecycle.md §4.2 carries the row; §1.4.2 carries the carve-out this
// action is the trail for.
const AuditActionProfileNamePlaceholderScrubbed AuditAction = "profile_name_placeholder_scrubbed"

// ProfileNamePlaceholderAuditor writes the audit trail for the MYR-583
// placeholder scrub (`cmd/backfill-name-confirmations`).
//
// WHY AN AUDIT ROW FOR A ONE-ROW UPDATE. CG-DL-3 names DELETEs, so nothing
// compels this — but the write is an operator mutating a row in the sibling app's
// `"User"` table out of band, and the difference between a provable change and
// somebody's terminal scrollback is exactly this row. It is also what makes the
// §1.4.2 carve-out auditable in the way every other §1.4 carve-out is.
//
// STRONGER THAN THE MYR-447 PRECEDENT, and that is worth stating: the passenger
// scrub could only promise "every audit row written and confirmed before the first
// column is cleared", because it was a long autocommit pass over a whole table.
// This scrub is bounded at a handful of rows and runs in ONE transaction, so the
// audit row and the write it describes commit together or not at all — the
// guarantee CG-DL-3 actually asks for.
type ProfileNamePlaceholderAuditor struct{}

// NewProfileNamePlaceholderAuditor builds the audit writer. It holds no pool: the
// caller supplies the transaction, because the whole point is that the audit row
// and the scrub share one.
func NewProfileNamePlaceholderAuditor() *ProfileNamePlaceholderAuditor {
	return &ProfileNamePlaceholderAuditor{}
}

// profileNamePlaceholderAuditMetadata is the P0-only metadata (CG-DL-5): the
// placeholder LITERAL and the source, no name and no email.
//
// The literal is safe to record and is the useful fact: 'Tesla User' names nobody
// — it is a machine-written string the legacy web onboarding seeded for every
// account it created — which is also the reason this scrub is a cleanup rather
// than an erasure of somebody's PII. Recording WHICH placeholder was cleared is
// what lets a future operator tell this pass apart from a hypothetical later one
// aimed at a different placeholder.
type profileNamePlaceholderAuditMetadata struct {
	Placeholder string `json:"placeholder"`
	Source      string `json:"source"`
}

// RecordPlaceholderScrub writes one audit row, inside the caller's transaction,
// for one account whose placeholder name is about to be cleared. Call it BEFORE
// the UPDATE (CG-DL-3's ordering) — on one transaction the ordering is belt to
// the braces, but the pattern is the pattern.
//
// It reuses the same-package queryAuditInsert column list, so CG-DL-8's
// cross-repo column coupling holds for this writer too.
func (a *ProfileNamePlaceholderAuditor) RecordPlaceholderScrub(
	ctx context.Context, tx pgx.Tx, userID, placeholder string,
) error {
	if userID == "" {
		return fmt.Errorf("store.RecordPlaceholderScrub: empty user id")
	}
	meta, err := json.Marshal(profileNamePlaceholderAuditMetadata{
		Placeholder: placeholder,
		Source:      "one_time_backfill",
	})
	if err != nil {
		return fmt.Errorf("store.RecordPlaceholderScrub: marshal audit metadata: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		newProvisionID(),
		userID,
		now,
		string(AuditActionProfileNamePlaceholderScrubbed),
		auditTargetTypeUser,
		userID,
		auditInitiatorOperator,
		meta,
		now,
	); err != nil {
		return fmt.Errorf("store.RecordPlaceholderScrub(user=%s): %w", userID, err)
	}
	return nil
}
