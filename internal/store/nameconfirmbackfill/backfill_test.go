package nameconfirmbackfill_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/nameconfirmbackfill"
)

// MYR-583's deploy-time data step, against a real Postgres.
//
// It has its own container rather than sharing internal/store's, matching
// accountbackfill and routeblobbackfill: the package is deliberately outside
// internal/store so the running server cannot import a destructive code path, and
// its test follows it out.
//
// The schema below is the MINIMUM the two statements touch — a Prisma-shaped
// `"User"`, the `"AuditLog"` the scrub's trail lands in, and the Go-owned
// confirmation table copied from migration 0042. Deliberately not the whole
// Prisma schema: what is under test is two predicates and a transaction boundary.

var (
	testPool        *pgxpool.Pool
	dockerAvailable bool
)

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker not available, skipping nameconfirmbackfill tests")
		os.Exit(m.Run())
	}
	ctx := context.Background()
	c, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("nameconfirm_test"),
		postgres.WithUsername("u"),
		postgres.WithPassword("p"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		os.Exit(1)
	}
	conn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conn string: %v\n", err)
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new pool: %v\n", err)
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		fmt.Fprintf(os.Stderr, "schema: %v\n", err)
		pool.Close()
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	testPool = pool
	dockerAvailable = true

	code := m.Run()
	pool.Close()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS "User" (
    "id"        TEXT PRIMARY KEY,
    "name"      TEXT,
    "email"     TEXT,
    "updatedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS "AuditLog" (
    "id"         TEXT PRIMARY KEY,
    "userId"     TEXT        NOT NULL,
    "timestamp"  TIMESTAMPTZ NOT NULL,
    "action"     TEXT        NOT NULL,
    "targetType" TEXT        NOT NULL,
    "targetId"   TEXT        NOT NULL,
    "initiator"  TEXT        NOT NULL,
    "metadata"   JSONB       DEFAULT '{}',
    "createdAt"  TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS go_profile_name_confirmations (
    user_id      TEXT        NOT NULL PRIMARY KEY,
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`

func isDockerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil //#nosec G204 -- hardcoded
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seed installs the four account shapes the run has to tell apart, and truncates
// first so the arms cannot inherit each other's rows.
func seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`TRUNCATE "User"`,
		`TRUNCATE "AuditLog"`,
		`TRUNCATE go_profile_name_confirmations`,
	} {
		if _, err := testPool.Exec(ctx, q); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}
	inWindow := nameconfirmbackfill.ConfirmationWindowStart.Add(2 * time.Hour)
	before := nameconfirmbackfill.ConfirmationWindowStart.Add(-90 * 24 * time.Hour)

	rows := []struct {
		id        string
		name      *string
		updatedAt time.Time
	}{
		// Renamed through the prompt after the endpoint shipped — the population
		// the backfill exists for.
		{id: "usr_confirmer", name: strptr("Amruth Kelkar"), updatedAt: inWindow},
		// A name from before the endpoint existed: legacy, unconfirmed, and it
		// must STAY unconfirmed. This is the whole point of the window.
		{id: "usr_legacy", name: strptr("Nabil Rahman"), updatedAt: before},
		// The placeholder account (James's 2026-03 web-era row). Out of window
		// too, so the scrub is what deals with it.
		{id: "usr_placeholder", name: strptr(nameconfirmbackfill.LegacyWebPlaceholderName), updatedAt: before},
		// Touched inside the window but nameless — nothing to have confirmed.
		{id: "usr_nameless", name: nil, updatedAt: inWindow},
		// Whitespace-only inside the window: NOT a name at any rung, so not a
		// confirmation either.
		{id: "usr_blank", name: strptr("   "), updatedAt: inWindow},
	}
	for _, r := range rows {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "User" ("id", "name", "updatedAt") VALUES ($1, $2, $3)`,
			r.id, r.name, r.updatedAt); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

func TestRunner_Run(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()

	t.Run("a dry run reports the work and writes NOTHING", func(t *testing.T) {
		seed(t)
		res, err := nameconfirmbackfill.New(testPool, quietLogger(), true).Run(ctx)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.DryRun {
			t.Error("the report must say it was a dry run")
		}
		if res.PlaceholdersScrubbed != 1 {
			t.Errorf("placeholdersScrubbed = %d, want 1", res.PlaceholdersScrubbed)
		}
		if res.ConfirmationsBackfilled != 1 {
			t.Errorf("confirmationsBackfilled = %d, want 1 (only the in-window named account)",
				res.ConfirmationsBackfilled)
		}
		// Nothing committed: the placeholder is intact and no confirmation exists.
		if got := nameOf(t, "usr_placeholder"); got == nil || *got != nameconfirmbackfill.LegacyWebPlaceholderName {
			t.Errorf("a dry run altered the placeholder name: %v", got)
		}
		if n := confirmationCount(t); n != 0 {
			t.Errorf("a dry run wrote %d confirmations", n)
		}
		if n := auditCount(t); n != 0 {
			t.Errorf("a dry run wrote %d audit rows", n)
		}
	})

	t.Run("a real run needs an auditor", func(t *testing.T) {
		seed(t)
		_, err := nameconfirmbackfill.New(testPool, quietLogger(), false).Run(ctx)
		if err == nil {
			t.Fatal("a real run with no auditor must be refused — an operator write to " +
				"the sibling app's User table may not go unrecorded")
		}
		if n := confirmationCount(t); n != 0 {
			t.Errorf("the refused run still wrote %d confirmations", n)
		}
	})

	t.Run("a real run scrubs the placeholder, audits it, and credits only the confirmers", func(t *testing.T) {
		seed(t)
		res, err := realRunner().Run(ctx)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.PlaceholdersScrubbed != 1 || res.ConfirmationsBackfilled != 1 {
			t.Fatalf("result = %+v, want 1 scrubbed and 1 backfilled", res)
		}
		if got := nameOf(t, "usr_placeholder"); got != nil {
			t.Errorf("the placeholder survived as %q — it must become NULL", *got)
		}

		// The audit trail, and its shape: P0 metadata only, filed against the
		// account whose column changed.
		var action, initiator, targetType, targetID, meta string
		if err := testPool.QueryRow(ctx,
			`SELECT "action", "initiator", "targetType", "targetId", "metadata"::text
			 FROM "AuditLog" WHERE "userId" = $1`, "usr_placeholder").
			Scan(&action, &initiator, &targetType, &targetID, &meta); err != nil {
			t.Fatalf("read audit row: %v", err)
		}
		if action != string(store.AuditActionProfileNamePlaceholderScrubbed) {
			t.Errorf("audit action = %q", action)
		}
		if initiator != "operator" || targetType != "user" || targetID != "usr_placeholder" {
			t.Errorf("audit row = initiator %q, targetType %q, targetId %q", initiator, targetType, targetID)
		}

		// EXACTLY the confirmers, and nobody else. Each exclusion is a separate
		// rule the predicate has to get right.
		for _, tc := range []struct {
			id     string
			want   bool
			reason string
		}{
			{id: "usr_confirmer", want: true, reason: "named inside the window — used the prompt"},
			{id: "usr_legacy", want: false, reason: "named before the endpoint existed"},
			{id: "usr_placeholder", want: false, reason: "a placeholder is not a confirmed name"},
			{id: "usr_nameless", want: false, reason: "no name to have confirmed"},
			{id: "usr_blank", want: false, reason: "whitespace is not a name at any rung"},
		} {
			if got := confirmed(t, tc.id); got != tc.want {
				t.Errorf("%s: confirmed = %v, want %v (%s)", tc.id, got, tc.want, tc.reason)
			}
		}

		// confirmed_at carries the row's OWN updatedAt, not the run's clock, so a
		// backfilled row records when the person actually confirmed rather than
		// when an operator ran a binary. Asserted by equality rather than by
		// "recent enough", because the window bound is a fixed date and a
		// wall-clock comparison would pass or fail depending on the hour CI runs.
		at := confirmedAt(t, "usr_confirmer")
		if at == nil {
			t.Fatal("the confirmer has no timestamp")
		}
		if want := seededUpdatedAt(t, "usr_confirmer"); !at.Equal(want) {
			t.Errorf("confirmed_at = %v, want the row's own updatedAt %v — a backfilled "+
				"row must not be stamped with the run's clock", at, want)
		}
	})

	t.Run("re-running changes nothing", func(t *testing.T) {
		seed(t)
		if _, err := realRunner().Run(ctx); err != nil {
			t.Fatalf("first Run: %v", err)
		}
		res, err := realRunner().Run(ctx)
		if err != nil {
			t.Fatalf("second Run: %v", err)
		}
		if res.PlaceholdersScrubbed != 0 || res.ConfirmationsBackfilled != 0 {
			t.Errorf("second run = %+v, want zeros — the step must be idempotent", res)
		}
		if n := auditCount(t); n != 1 {
			t.Errorf("audit rows = %d after two runs, want 1 — a no-op scrub must not audit", n)
		}
	})

	// An observed confirmation must never be replaced by an inferred one.
	t.Run("an existing confirmation is not overwritten", func(t *testing.T) {
		seed(t)
		real := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_profile_name_confirmations (user_id, confirmed_at) VALUES ($1, $2)`,
			"usr_confirmer", real); err != nil {
			t.Fatalf("seed existing confirmation: %v", err)
		}
		if _, err := realRunner().Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		at := confirmedAt(t, "usr_confirmer")
		if at == nil || !at.Equal(real) {
			t.Errorf("confirmed_at = %v, want the pre-existing %v — ON CONFLICT DO NOTHING", at, real)
		}
	})
}

func realRunner() *nameconfirmbackfill.Runner {
	return nameconfirmbackfill.New(testPool, quietLogger(), false).
		WithAuditor(store.NewProfileNamePlaceholderAuditor())
}

func strptr(s string) *string { return &s }

// seededUpdatedAt reads the row's `"updatedAt"` — the value the backfill copies
// into `confirmed_at`.
func seededUpdatedAt(t *testing.T, id string) time.Time {
	t.Helper()
	var at time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT "updatedAt" FROM "User" WHERE "id" = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read updatedAt(%s): %v", id, err)
	}
	return at
}

func nameOf(t *testing.T, id string) *string {
	t.Helper()
	var name *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT "name" FROM "User" WHERE "id" = $1`, id).Scan(&name); err != nil {
		t.Fatalf("read name(%s): %v", id, err)
	}
	return name
}

func confirmed(t *testing.T, id string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM go_profile_name_confirmations WHERE user_id = $1)`, id).
		Scan(&ok); err != nil {
		t.Fatalf("read confirmation(%s): %v", id, err)
	}
	return ok
}

func confirmedAt(t *testing.T, id string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT MAX(confirmed_at) FROM go_profile_name_confirmations WHERE user_id = $1`, id).
		Scan(&at); err != nil {
		t.Fatalf("read confirmed_at(%s): %v", id, err)
	}
	return at
}

func confirmationCount(t *testing.T) int {
	t.Helper()
	return countRows(t, `SELECT COUNT(*) FROM go_profile_name_confirmations`)
}

func auditCount(t *testing.T) int {
	t.Helper()
	return countRows(t, `SELECT COUNT(*) FROM "AuditLog"`)
}

func countRows(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
