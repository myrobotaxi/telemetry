package identity

import "log/slog"

// auditLogger emits the identity module's auth audit trail as structured slog
// records (ADR-001 §6). It records the event and the opaque user/family ids
// only — never PII (no email, name, raw token, or token hash). This trail
// stays inside the module rather than touching the Prisma-owned "AuditLog"
// enum (which is contract-restricted to three system actions).
type auditLogger struct {
	log *slog.Logger
}

func newAuditLogger(log *slog.Logger) *auditLogger {
	if log == nil {
		log = slog.Default()
	}
	return &auditLogger{log: log.With(slog.String("audit", "identity"))}
}

// signIn records a successful Apple sign-in. newUser distinguishes a freshly
// minted account from a returning/linked one.
func (a *auditLogger) signIn(userID string, newUser bool) {
	a.log.Info("auth.sign_in",
		slog.String("event", "sign_in"),
		slog.String("user_id", userID),
		slog.Bool("new_user", newUser),
	)
}

// refresh records a successful refresh-token rotation.
func (a *auditLogger) refresh(userID string) {
	a.log.Info("auth.refresh",
		slog.String("event", "refresh"),
		slog.String("user_id", userID),
	)
}

// reuseDetected records a refresh-token reuse (theft) that revoked a family.
func (a *auditLogger) reuseDetected(userID, familyID string) {
	a.log.Warn("auth.reuse_detected",
		slog.String("event", "reuse_detected"),
		slog.String("user_id", userID),
		slog.String("family_id", familyID),
	)
}

// revoke records an explicit sign-out (family revocation).
func (a *auditLogger) revoke(userID string) {
	a.log.Info("auth.revoke",
		slog.String("event", "revoke"),
		slog.String("user_id", userID),
	)
}
