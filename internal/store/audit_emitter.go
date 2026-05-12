package store

import (
	"context"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/mask"
)

// MaskAuditEmitter adapts AuditRepo to the mask.AuditEmitter
// interface (MYR-71). The mask package defines its own AuditEntry
// type so internal/mask does not depend on internal/store; this
// adapter copies the cross-package fields one-by-one and forwards
// to AuditRepo.InsertAuditLog.
//
// The mask.AuditEntry and store.AuditEntry shapes are deliberately
// independent — drift between them is detected at compile time
// because every field is named explicitly in convertAuditEntry.
//
// Usage from cmd/telemetry-server/main.go:
//
//	auditRepo := store.NewAuditRepo(db.Pool())
//	emitter   := store.NewMaskAuditEmitter(auditRepo)
//	hub := ws.NewHub(logger, metrics, ws.WithMaskAudit(emitter, ...))
type MaskAuditEmitter struct {
	repo *AuditRepo
}

// Compile-time check that *MaskAuditEmitter satisfies the contract.
var _ mask.AuditEmitter = (*MaskAuditEmitter)(nil)

// NewMaskAuditEmitter wraps an AuditRepo so it can be passed where
// mask.AuditEmitter is expected.
func NewMaskAuditEmitter(repo *AuditRepo) *MaskAuditEmitter {
	return &MaskAuditEmitter{repo: repo}
}

// InsertAuditLog converts the cross-package mask.AuditEntry into a
// store.AuditEntry and forwards to AuditRepo. Any field rename in
// either struct breaks this conversion at compile time, which is
// exactly the drift-detection we want.
func (e *MaskAuditEmitter) InsertAuditLog(ctx context.Context, entry mask.AuditEntry) error {
	if e.repo == nil {
		return fmt.Errorf("store.MaskAuditEmitter.InsertAuditLog: nil repo")
	}
	return e.repo.InsertAuditLog(ctx, convertAuditEntry(entry))
}

// convertAuditEntry copies fields from mask.AuditEntry to
// store.AuditEntry. The store.AuditAction conversion preserves the
// opaque string — invalid actions land at the DB level (the column
// is TEXT with no enum constraint at the SQL layer), but the closed
// enums on both sides keep this safe in practice.
func convertAuditEntry(entry mask.AuditEntry) AuditEntry {
	return AuditEntry{
		ID:         entry.ID,
		UserID:     entry.UserID,
		Timestamp:  entry.Timestamp,
		Action:     AuditAction(entry.Action),
		TargetType: entry.TargetType,
		TargetID:   entry.TargetID,
		Initiator:  entry.Initiator,
		Metadata:   entry.Metadata,
		CreatedAt:  entry.CreatedAt,
	}
}
