// Audit sidecar wiring helpers split out of wiring.go to keep each file
// under the CLAUDE.md 300-line cap. These functions are called by run() in
// main.go — they are not standalone entry points.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/auditsidecar"
)

// buildAuditRepo constructs an AuditRepo wired with the appropriate sidecar.
// It calls setupAuditSidecar internally; on success, the sidecar Close is
// registered with a background goroutine tied to ctx so the queue drains
// before the process exits. This extraction keeps run()'s cyclomatic
// complexity within the cyclop limit.
func buildAuditRepo(
	ctx context.Context,
	reg prometheus.Registerer,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) (*store.AuditRepo, error) {
	sidecar, closeFn, err := setupAuditSidecar(ctx, reg, logger)
	if err != nil {
		return nil, fmt.Errorf("setting up audit sidecar: %w", err)
	}
	if closeFn != nil {
		// Register a background goroutine that drains the sidecar when ctx
		// is cancelled (i.e., when the process receives SIGINT/SIGTERM).
		// context.Background() is intentional here: the drain timeout must
		// outlive the cancelled process context.
		go func() { //nolint:gosec // G118: intentional use of Background for post-cancel drain
			<-ctx.Done()
			closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if cerr := closeFn(closeCtx); cerr != nil {
				logger.Warn("audit sidecar close error", slog.String("error", cerr.Error()))
			}
		}()
	}
	return store.NewAuditRepoWithSidecar(pool, sidecar, logger.With(slog.String("component", "audit-repo"))), nil
}

// setupAuditSidecar reads AUDIT_SIDECAR_BUCKET and AUDIT_SIDECAR_REGION
// (default us-east-1) and returns either a live S3Sidecar or a NoopSidecar.
//
// Mode A (AUDIT_SIDECAR_BUCKET empty — local dev):
//   - Returns auditsidecar.NoopSidecar{}, nil, nil.
//   - A startup warning is logged so ops knows the sidecar is disabled.
//   - No metrics are registered, no goroutines are started.
//
// Mode B (AUDIT_SIDECAR_BUCKET set — production):
//   - Constructs an AWSS3Putter using the ambient IAM role (IAM instance
//     profile, ECS task role, or AWS_* env vars). The service role must hold
//     s3:PutObject on the sidecar bucket ONLY — see
//     deployments/terraform/audit-sidecar/iam.tf.
//   - Registers audit_sidecar_writes_total, audit_sidecar_write_failures_total,
//     and audit_sidecar_queue_depth on reg.
//   - Starts the background worker goroutine. The returned closeFn must be
//     called during graceful shutdown (before the process exits) to drain.
//
// Sidecar failure NEVER fails AuditRepo.InsertAuditLog — the DB INSERT is
// canonical; the sidecar is best-effort at-most-once (see
// docs/operations/backup-retention.md §2.1 and internal/store/auditsidecar
// package comment).
func setupAuditSidecar(
	ctx context.Context,
	reg prometheus.Registerer,
	logger *slog.Logger,
) (sc auditsidecar.Sidecar, closeFn func(context.Context) error, err error) {
	bucket := os.Getenv("AUDIT_SIDECAR_BUCKET")
	if bucket == "" {
		logger.Warn("AUDIT_SIDECAR_BUCKET not set — audit sidecar disabled (no-op); " +
			"set AUDIT_SIDECAR_BUCKET to enable S3 mirroring for backup-retention runbook §2")
		return auditsidecar.NoopSidecar{}, nil, nil
	}

	region := os.Getenv("AUDIT_SIDECAR_REGION")
	if region == "" {
		region = "us-east-1"
	}

	putter, err := auditsidecar.NewAWSS3Putter(ctx, region)
	if err != nil {
		return nil, nil, fmt.Errorf("constructing audit sidecar S3 putter: %w", err)
	}

	m := auditsidecar.NewPrometheusMetrics(reg)
	s := auditsidecar.NewS3Sidecar(auditsidecar.S3SidecarConfig{Bucket: bucket}, putter, m,
		logger.With(slog.String("component", "audit-sidecar")))

	logger.Info("audit sidecar enabled",
		slog.String("bucket", bucket),
		slog.String("region", region))

	return s, func(shutdownCtx context.Context) error {
		return s.Close(shutdownCtx)
	}, nil
}
