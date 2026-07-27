package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver
	_ "github.com/golang-migrate/migrate/v4/source/file"     // file:// source
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// newTestMigrator builds a migrator over the on-disk migrations directory (this
// test file sits beside it). RunMigrations only ever goes UP, so exercising the
// down-migration needs a migrator of our own rather than new production surface.
func newTestMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	url := testConnStr
	if after, ok := strings.CutPrefix(url, "postgres://"); ok {
		url = "pgx5://" + after
	}
	m, err := migrate.New("file://migrations", url)
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	return m
}

// migration0015Columns is every column migration 0015 adds, with the type the
// up-migration declares. The types are asserted, not just presence: BIGINT for
// the ms counters (a ms counter must not sit one firmware bug away from
// overflow) and DOUBLE PRECISION for media_volume_max (the contract types the
// volume ceiling as `number`, matching the media_volume column it bounds, so
// mediaVolume / mediaVolumeMax stays exact).
var migration0015Columns = map[string]string{
	"media_now_playing_title":       "text",
	"media_now_playing_artist":      "text",
	"media_now_playing_album":       "text",
	"media_now_playing_station":     "text",
	"media_playback_source":         "text",
	"media_now_playing_duration_ms": "bigint",
	"media_now_playing_elapsed_ms":  "bigint",
	"media_volume_max":              "double precision",
	"seat_cooling_capable":          "boolean",
}

func columnTypes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT column_name, data_type, is_nullable
		 FROM information_schema.columns
		 WHERE table_name = 'go_vehicle_control_state'`)
	if err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	defer rows.Close()

	got := make(map[string]string)
	for rows.Next() {
		var name, dataType, nullable string
		if err := rows.Scan(&name, &dataType, &nullable); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ours := migration0015Columns[name]; ours && nullable != "YES" {
			t.Errorf("%s: is_nullable = %s, want YES — NULL is the honest-unknown "+
				"'never read' marker for every column in this table", name, nullable)
		}
		got[name] = dataType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

// TestMigration0015_UpAddsMediaColumns verifies the up-migration lands all nine
// columns with the right types and nullability. RunMigrations applies the whole
// chain, so this also proves 0015 sequences cleanly after the 0014 head.
func TestMigration0015_UpAddsMediaColumns(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	ensureControlMigration(t)

	got := columnTypes(t, testPool)
	for name, wantType := range migration0015Columns {
		gotType, ok := got[name]
		if !ok {
			t.Errorf("%s: column missing after migrate up", name)
			continue
		}
		if gotType != wantType {
			t.Errorf("%s: data_type = %q, want %q", name, gotType, wantType)
		}
	}
}

// TestMigration0015_DownDropsMediaColumns exercises the down-migration and then
// restores the schema. It asserts the down is SURGICAL: the nine new columns go
// away and every earlier issue's columns survive, so a rollback of MYR-303/308
// cannot take MYR-269/273/274/279/298 state with it.
//
// Runs migrate down one step then back up, leaving the database at head for
// whatever runs next.
func TestMigration0015_DownDropsMediaColumns(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	ensureControlMigration(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	got := columnTypes(t, testPool)
	for name := range migration0015Columns {
		if _, ok := got[name]; ok {
			t.Errorf("%s: column still present after migrate down", name)
		}
	}

	// Surgical: earlier issues' columns must survive an 0015 rollback.
	for _, survivor := range []string{
		"is_locked",             // MYR-269
		"media_volume",          // MYR-273
		"trim",                  // MYR-279
		"hvac_auto_mode",        // MYR-274
		"seat_vent_enabled",     // MYR-298
		"media_playback_status", // MYR-298
	} {
		if _, ok := got[survivor]; !ok {
			t.Errorf("%s: dropped by the 0015 down-migration — the rollback must be surgical", survivor)
		}
	}
}
