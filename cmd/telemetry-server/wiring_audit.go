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
		registerSidecarDrainGoroutine(ctx, closeFn, logger)
	}
	return store.NewAuditRepoWithSidecar(pool, sidecar, logger.With(slog.String("component", "audit-repo"))), nil
}

// setupAuditSidecar reads AUDIT_SIDECAR_BUCKET / _REGION / _ENDPOINT and
// returns either a live S3Sidecar or a NoopSidecar.
//
// Mode A (AUDIT_SIDECAR_BUCKET empty — local dev):
//   - Returns auditsidecar.NoopSidecar{}, nil, nil.
//   - A startup warning is logged so ops knows the sidecar is disabled.
//   - No metrics are registered, no goroutines are started.
//
// Mode B (AUDIT_SIDECAR_BUCKET set — production):
//   - Constructs an S3Putter pointed at AUDIT_SIDECAR_ENDPOINT when that
//     env var is set (Supabase Storage:
//     `https://<project>.supabase.co/storage/v1/s3`). When empty the
//     putter uses the default AWS regional endpoint.
//   - Authenticates via AUDIT_SIDECAR_ACCESS_KEY +
//     AUDIT_SIDECAR_SECRET_KEY when both are set (the Supabase Storage
//     S3 access key — Supabase issues these from Storage → S3
//     connection; **no AWS account required**, the AWS SDK is just the
//     client library). Falls back to the ambient AWS credential chain
//     if those env vars are unset, so an AWS-deployed configuration
//     still works.
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
			"set AUDIT_SIDECAR_BUCKET to enable Supabase Storage mirroring for backup-retention runbook §2")
		return auditsidecar.NoopSidecar{}, nil, nil
	}

	region := os.Getenv("AUDIT_SIDECAR_REGION")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := os.Getenv("AUDIT_SIDECAR_ENDPOINT")
	accessKey := os.Getenv("AUDIT_SIDECAR_ACCESS_KEY")
	secretKey := os.Getenv("AUDIT_SIDECAR_SECRET_KEY")

	putter, err := auditsidecar.NewS3Putter(ctx, auditsidecar.PutterConfig{
		Region:          region,
		Endpoint:        endpoint,
		UsePathStyle:    endpoint != "", // Supabase Storage requires path-style.
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("constructing audit sidecar S3 putter: %w", err)
	}

	m := auditsidecar.NewPrometheusMetrics(reg)
	s := auditsidecar.NewS3Sidecar(auditsidecar.S3SidecarConfig{Bucket: bucket}, putter, m,
		logger.With(slog.String("component", "audit-sidecar")))

	backend := "aws-s3"
	if endpoint != "" {
		backend = "s3-compatible"
	}
	logger.Info("audit sidecar enabled",
		slog.String("bucket", bucket),
		slog.String("region", region),
		slog.String("endpoint", endpoint),
		slog.String("backend", backend))

	return s, func(shutdownCtx context.Context) error {
		return s.Close(shutdownCtx)
	}, nil
}

// registerSidecarDrainGoroutine spawns a background goroutine that calls
// closeFn after ctx cancels (i.e., on SIGINT/SIGTERM), giving the
// sidecar worker a fresh context with a finite deadline to drain.
//
// The goroutine deliberately derives its drain context from
// `context.Background()` rather than a request-scoped ancestor — at
// this point the parent ctx is already cancelled, and the whole point
// of the drain is to outlive that cancellation. Linters that warn
// about "context.Background in goroutines while a request-scoped
// context is in scope" (gosec G117/G118, golangci-lint contextcheck)
// are wrong here — we extract the goroutine into its own function so
// the suppression markers attach to a single, narrowly-scoped
// statement instead of being threaded through buildAuditRepo.
//
//nolint:gosec,contextcheck // intentional Background for post-cancel drain
func registerSidecarDrainGoroutine(
	ctx context.Context,
	closeFn func(context.Context) error,
	logger *slog.Logger,
) {
	// #nosec G117,G118 -- intentional Background for post-cancel drain
	go func() {
		<-ctx.Done()
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := closeFn(closeCtx); err != nil {
			logger.Warn("audit sidecar close error", slog.String("error", err.Error()))
		}
	}()
}
