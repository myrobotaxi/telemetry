package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// TestRunMigrations_Apply verifies that:
//  1. RunMigrations succeeds on a clean database (applies pending migrations).
//  2. Running RunMigrations a second time is a no-op (idempotent -- ErrNoChange
//     is treated as success and the function returns nil).
//
// The testcontainers-based Postgres instance is provided by TestMain in
// db_test.go. When Docker is unavailable the entire suite exits early, so
// this test is safe to skip in CI environments without Docker.
func TestRunMigrations_Apply(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping migration integration test")
	}

	ctx := context.Background()
	logger := testLogger()

	// First application -- should succeed and apply 0001_init.
	if err := store.RunMigrations(ctx, testConnStr, logger); err != nil {
		t.Fatalf("RunMigrations (first run) failed: %v", err)
	}

	// Second application -- should be a no-op (ErrNoChange swallowed as nil).
	if err := store.RunMigrations(ctx, testConnStr, logger); err != nil {
		t.Fatalf("RunMigrations (second run / idempotency check) failed: %v", err)
	}
}

// TestRunMigrations_BadURL verifies that RunMigrations returns an error for
// an unreachable database URL and does not panic.
func TestRunMigrations_BadURL(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	err := store.RunMigrations(ctx, "postgres://bad:bad@localhost:19999/nope?sslmode=disable", logger) // #nosec G101 -- test credentials
	if err == nil {
		t.Fatal("expected an error for unreachable database, got nil")
	}

	// ErrNoChange is not expected here -- any real error is fine.
	if errors.Is(err, migrate.ErrNoChange) {
		t.Fatal("expected a connection error, not ErrNoChange")
	}
}
