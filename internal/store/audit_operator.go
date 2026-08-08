package store

// OPERATOR ACCESS AUDIT (MYR-447)
// ===============================
// Every operator tool that DECRYPTS user data must leave a trail. Before
// MYR-447 the `ops` CLI could print a user's Tesla access/refresh token or
// a fully decrypted Vehicle row — GPS, geocoded addresses, the nav route
// polyline — and the only record that it happened was the operator's own
// terminal scrollback.
//
// This file adds the write side of that trail. It deliberately does NOT
// live in audit_repo.go: that file mirrors the Prisma-owned column list
// (CG-DL-8) and its AuditRepo serves the SERVER's system-initiated rows.
// Operator access is a separate concern with a separate actor model, so
// it gets a separate type over the same table and the same INSERT.
//
// Two structural gaps this file closes, both worth stating plainly:
//
//  1. `initiator` had no value for a human operator. data-lifecycle.md
//     §4.2 enumerated only `user`, `system_pruner`, `system_auth`. An
//     operator is none of those: not the data subject acting on their own
//     data, and not an unattended job. auditInitiatorOperator is new.
//
//  2. AuditEntry has no column for a non-user ACTOR. `userId` is the DATA
//     SUBJECT — that is what makes "show me everything that touched this
//     person's data" a single indexed lookup, and it is why the column
//     survives the subject's deletion (§4.5). The operator's identity
//     therefore rides in `metadata.operator`, not in `userId`.
//
// Classification (CG-DL-5): metadata carries the command name, the
// operator handle and the NAMES of the fields decrypted. Never a value.
// A field NAME is an enum-like identifier drawn from a fixed vocabulary
// (column names); a field VALUE is the P1 payload the audit exists to
// account for. Writing "latitude" is the point; writing 37.7749 would
// reproduce inside the audit trail the exact leak being recorded.
// validateFieldName below enforces that structurally rather than by
// convention — see its comment.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// AuditActionOperatorDecrypt records an operator tool decrypting user
	// data (MYR-447). One row per invocation of a decrypting subcommand,
	// written BEFORE the plaintext is printed or transmitted. Unlike the
	// other actions in this package it records a READ, not a mutation:
	// nothing about the subject's data changed, only who saw it.
	AuditActionOperatorDecrypt AuditAction = "operator_decrypt"

	// auditInitiatorOperator marks an action initiated by a human operator
	// running internal tooling, attributed by the OPS_OPERATOR handle in
	// metadata. Distinct from `user` (the data subject acting on their own
	// data) and from the `system_*` initiators (unattended jobs).
	auditInitiatorOperator = "operator"
)

// OperatorTargetType narrows AuditEntry.TargetType to the values an
// operator access row may carry. Both reuse the existing §4.2 vocabulary
// rather than inventing new target types.
type OperatorTargetType string

const (
	// OperatorTargetUser pairs with a targetId that is the subject's cuid —
	// used when the decrypted material hangs off the account (OAuth tokens).
	OperatorTargetUser OperatorTargetType = auditTargetTypeUser
	// OperatorTargetVehicle pairs with a targetId that is the Vehicle cuid.
	// Note it is the cuid and NOT the VIN: audit columns are P0 and the VIN
	// is restricted to last-4 outside the owner's own snapshot
	// (data-classification.md §1.3, §2.1).
	OperatorTargetVehicle OperatorTargetType = auditTargetTypeVehicle
)

// Errors returned by RecordDecrypt. All of them are fail-closed: the
// caller must abort the decrypt rather than proceed unaudited.
var (
	// ErrOperatorHandleRequired means OPS_OPERATOR was unset, blank, or not
	// a plausible handle. An unattributable decrypt is the exact thing
	// MYR-447 exists to prevent, so there is no default and no fallback to
	// the OS username (which is trivially spoofed and often "root").
	ErrOperatorHandleRequired = errors.New("operator handle is required")

	// ErrOperatorAccessInvalid means the access record was incomplete or
	// carried something that must not be written to a P0 column.
	ErrOperatorAccessInvalid = errors.New("invalid operator access record")

	// ErrOperatorAuditorNotConfigured means RecordDecrypt was called on a
	// zero-value auditor. Returned rather than panicking so a miswired
	// command still fails closed.
	ErrOperatorAuditorNotConfigured = errors.New("operator auditor has no connection pool")
)

// operatorIdentifierMax bounds both the operator handle and each field
// name. Every legitimate value is a short identifier; a Tesla JWT, a
// polyline or a street address is not.
const operatorIdentifierMax = 64

// operatorHandlePattern accepts a staff handle and REJECTS an email
// address (no `@`). That exclusion is the reason metadata.operator can be
// classified P0: a handle like `tnandola` is an internal directory key
// that means nothing outside the company, whereas `t.nandola@example.com`
// is a routable personal identifier — P1 by the same logic that makes
// User.email P1 in data-classification.md §1.1. Keeping the P0 promise of
// the AuditLog table intact is worth the small friction of rejecting the
// email form with a message that says so.
var operatorHandlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// operatorFieldPattern accepts a column/field identifier only. This is
// the structural half of CG-DL-5 enforcement: a caller cannot smuggle a
// decrypted VALUE into the P0 metadata by passing it as a "field name",
// because the shapes P1 values take do not match. A coordinate
// ("37.7749") starts with a digit-then-dot but is caught by the leading
// letter requirement; an address ("123 Main St") has spaces and a leading
// digit; a JWT exceeds operatorIdentifierMax. Names may be dotted so a
// caller can qualify a nested field (e.g. "vehicle.latitude").
var operatorFieldPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)*$`)

// OperatorAccess describes a single operator-initiated decryption of user
// data. It is deliberately all-P0: identifiers, an enum, a command name
// and field names.
type OperatorAccess struct {
	// Operator is the OPS_OPERATOR handle — WHO. Lands in metadata, not in
	// the userId column (see the file header).
	Operator string
	// Command is the invoked subcommand, e.g. "ops auth token" — WHAT tool.
	Command string
	// UserID is the DATA SUBJECT whose plaintext was exposed, matching the
	// meaning AuditLog.userId carries everywhere else in this package.
	UserID string
	// TargetType / TargetID identify the affected record (§4.2).
	TargetType OperatorTargetType
	TargetID   string
	// Fields names the decrypted fields — WHAT data. Names only, never
	// values; order is normalized on write so rows compare cleanly.
	Fields []string
}

// operatorAuditMetadata is the metadata JSONB shape for an
// operator_decrypt row. Every key is P0:
//
//	command    — a fixed subcommand string from the CLI's own vocabulary
//	operator   — an internal staff handle, never an email (see the pattern)
//	fields     — column identifiers, never their values
//	fieldCount — an aggregate count, the canonical P0 metadata shape
//
// fieldCount is redundant with len(fields) by construction and is kept
// anyway: it makes "which decrypt exposed the most?" a scalar comparison
// for a reviewer querying this table, without JSONB array arithmetic.
type operatorAuditMetadata struct {
	Command    string   `json:"command"`
	Operator   string   `json:"operator"`
	Fields     []string `json:"fields"`
	FieldCount int      `json:"fieldCount"`
}

// OperatorAuditor appends operator-access rows to the Prisma-owned
// AuditLog table. Like AuditRepo it is insert-only by design — see the
// append-only invariant note at the top of audit_repo.go.
//
// It does not mirror to the MYR-77 S3 sidecar. The sidecar is best-effort
// and at-most-once; an operator-access row must be fail-closed, and mixing
// a droppable path into a mandatory one invites confusion about which
// guarantee applies. The DB row is canonical.
type OperatorAuditor struct {
	pool *pgxpool.Pool
}

// NewOperatorAuditor creates an auditor backed by the given pool.
func NewOperatorAuditor(pool *pgxpool.Pool) *OperatorAuditor {
	return &OperatorAuditor{pool: pool}
}

// RecordDecrypt appends exactly one operator_decrypt row.
//
// FAIL-CLOSED CONTRACT: callers MUST invoke this BEFORE the plaintext is
// printed, written, or transmitted, and MUST abort on error. This mirrors
// CG-DL-3's posture for deletions ("audit FIRST, in the same transaction,
// so a failure to record the deletion prevents the deletion"). There is no
// transaction to share here — the read is not a DB mutation — so ordering
// is what enforces it: a returned error means the access was never
// recorded, and an unrecorded access must therefore not happen.
func (a *OperatorAuditor) RecordDecrypt(ctx context.Context, access OperatorAccess) error {
	if a == nil || a.pool == nil {
		return fmt.Errorf("store.OperatorAuditor.RecordDecrypt: %w", ErrOperatorAuditorNotConfigured)
	}
	if err := access.validate(); err != nil {
		return fmt.Errorf("store.OperatorAuditor.RecordDecrypt: %w", err)
	}

	// Copy before sorting: the caller's slice is often a package-level
	// vocabulary constant shared across invocations.
	fields := make([]string, len(access.Fields))
	copy(fields, access.Fields)
	sort.Strings(fields)

	meta, err := json.Marshal(operatorAuditMetadata{
		Command:    access.Command,
		Operator:   access.Operator,
		Fields:     fields,
		FieldCount: len(fields),
	})
	if err != nil {
		return fmt.Errorf("store.OperatorAuditor.RecordDecrypt: marshal metadata: %w", err)
	}

	now := time.Now().UTC()
	if _, err := a.pool.Exec(ctx, queryAuditInsert,
		newProvisionID(),                   // id (cuid)
		access.UserID,                      // userId (the DATA SUBJECT)
		now,                                // timestamp
		string(AuditActionOperatorDecrypt), // action
		string(access.TargetType),          // targetType
		access.TargetID,                    // targetId
		auditInitiatorOperator,             // initiator
		meta,                               // metadata (P0: names + counts)
		now,                                // createdAt
	); err != nil {
		return fmt.Errorf("store.OperatorAuditor.RecordDecrypt: insert audit: %w", err)
	}
	return nil
}

// validate rejects anything that would produce an unattributable row or
// put a non-P0 value in a P0 column.
func (a OperatorAccess) validate() error {
	if err := ValidateOperatorHandle(a.Operator); err != nil {
		return err
	}
	if a.Command == "" {
		return fmt.Errorf("%w: command is empty", ErrOperatorAccessInvalid)
	}
	if a.UserID == "" {
		return fmt.Errorf("%w: userId (data subject) is empty", ErrOperatorAccessInvalid)
	}
	if a.TargetID == "" {
		return fmt.Errorf("%w: targetId is empty", ErrOperatorAccessInvalid)
	}
	switch a.TargetType {
	case OperatorTargetUser, OperatorTargetVehicle:
	default:
		return fmt.Errorf("%w: unknown targetType %q", ErrOperatorAccessInvalid, a.TargetType)
	}
	if len(a.Fields) == 0 {
		return fmt.Errorf("%w: fields is empty — record what was decrypted", ErrOperatorAccessInvalid)
	}
	for _, f := range a.Fields {
		if err := validateFieldName(f); err != nil {
			return err
		}
	}
	return nil
}

// ValidateOperatorHandle reports whether s is usable as metadata.operator.
// Exported so the CLI can reject a bad OPS_OPERATOR before it opens a
// database connection.
func ValidateOperatorHandle(s string) error {
	if s == "" {
		return ErrOperatorHandleRequired
	}
	if len(s) > operatorIdentifierMax {
		return fmt.Errorf("%w: handle exceeds %d characters", ErrOperatorHandleRequired, operatorIdentifierMax)
	}
	if !operatorHandlePattern.MatchString(s) {
		return fmt.Errorf(
			"%w: %q is not a handle — use a short internal handle (letters, digits, . _ -), not an email address",
			ErrOperatorHandleRequired, s)
	}
	return nil
}

// validateFieldName enforces the CG-DL-5 "names, never values" rule at
// the write site rather than trusting every future caller to remember it.
func validateFieldName(f string) error {
	if len(f) > operatorIdentifierMax {
		return fmt.Errorf("%w: field name exceeds %d characters — this looks like a value, not a name",
			ErrOperatorAccessInvalid, operatorIdentifierMax)
	}
	if !operatorFieldPattern.MatchString(f) {
		return fmt.Errorf("%w: %q is not a field name — metadata records field NAMES, never decrypted values",
			ErrOperatorAccessInvalid, f)
	}
	return nil
}
