package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// migration0023Columns is the go_saved_places shape MYR-321 installs, with the
// information_schema data_type each column must report.
//
// The COORDINATES ARE `text`, NOT `double precision`, and that is the whole
// point of asserting types here rather than mere presence: they hold base64
// AES-256-GCM ciphertext (NFR-3.23, data-classification.md §1.17), following
// the go_ride_requests encrypt-only precedent. A future edit that "fixed" these
// to a numeric type would store a person's home address in the clear and this
// map is what stops it.
var migration0023Columns = map[string]string{
	"user_id":    "text",
	"kind":       "text",
	"lat_enc":    "text",
	"lng_enc":    "text",
	"label":      "text",
	"created_at": "timestamp with time zone",
	"updated_at": "timestamp with time zone",
}

// savedPlaceColumnTypes introspects the installed go_saved_places columns.
func savedPlaceColumnTypes(t *testing.T) map[string]string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name, data_type
		 FROM information_schema.columns
		 WHERE table_name = 'go_saved_places'`)
	if err != nil {
		t.Fatalf("introspect go_saved_places columns: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			t.Fatalf("scan column row: %v", err)
		}
		got[name] = dataType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate column rows: %v", err)
	}
	return got
}

func cleanSavedPlaces(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_saved_places`); err != nil {
		t.Fatalf("clean go_saved_places: %v", err)
	}
}

func TestMigration0023_UpCreatesSavedPlaces(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	got := savedPlaceColumnTypes(t)
	for col, wantType := range migration0023Columns {
		actual, ok := got[col]
		if !ok {
			t.Errorf("column %s missing after migrate up", col)
			continue
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}
	if len(got) != len(migration0023Columns) {
		t.Errorf("go_saved_places has %d columns, want %d — an undocumented column is a classification gap",
			len(got), len(migration0023Columns))
	}
}

// TestMigration0023_UpEnforcesCompositePrimaryKey proves the arbiter the §7.20
// upsert depends on. Without PRIMARY KEY (user_id, kind) the ON CONFLICT clause
// has no target and the endpoint fails at runtime, not at deploy — and a bug
// could write a second 'home' that silently shadows the first.
func TestMigration0023_UpEnforcesCompositePrimaryKey(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	insert := `INSERT INTO go_saved_places (user_id, kind, lat_enc, lng_enc, label)
	           VALUES ($1,$2,$3,$4,$5)`

	if _, err := testPool.Exec(ctx, insert, "cmig0023u", "home", "ct-lat", "ct-lng", "Casa"); err != nil {
		t.Fatalf("seed home: %v", err)
	}
	// The SAME user may hold the OTHER kind — the key is composite.
	if _, err := testPool.Exec(ctx, insert, "cmig0023u", "work", "ct-lat", "ct-lng", "HQ"); err != nil {
		t.Fatalf("same user, different kind must be allowed: %v", err)
	}
	// A DIFFERENT user may hold the same kind.
	if _, err := testPool.Exec(ctx, insert, "cmig0023v", "home", "ct-lat", "ct-lng", "Their casa"); err != nil {
		t.Fatalf("different user, same kind must be allowed: %v", err)
	}
	// A second 'home' for the SAME user must not exist.
	if _, err := testPool.Exec(ctx, insert, "cmig0023u", "home", "ct-lat", "ct-lng", "Second casa"); err == nil {
		t.Fatal("a second 'home' for one user succeeded, want a unique violation")
	}
}

// TestMigration0023_UpRejectsUnknownKind pins the CHECK constraint: the slot
// set is closed, and adding a third is a deliberate schema change plus a
// contract enum member, not a write.
func TestMigration0023_UpRejectsUnknownKind(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanSavedPlaces(t)

	for _, kind := range []string{"gym", "Home", "WORK", ""} {
		_, err := testPool.Exec(context.Background(),
			`INSERT INTO go_saved_places (user_id, kind, lat_enc, lng_enc, label)
			 VALUES ($1,$2,$3,$4,$5)`,
			"cmig0023k", kind, "ct-lat", "ct-lng", "X")
		if err == nil {
			t.Errorf("insert with kind=%q succeeded, want a CHECK violation", kind)
		}
	}
}

// TestMigration0023_UpRejectsBlankAndOverlongLabel pins the label CHECK. The
// endpoint validates first, so this is the backstop — but a backstop that does
// not hold is not one.
func TestMigration0023_UpRejectsBlankAndOverlongLabel(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	insert := `INSERT INTO go_saved_places (user_id, kind, lat_enc, lng_enc, label)
	           VALUES ($1,'home',$2,$3,$4)`

	if _, err := testPool.Exec(ctx, insert, "cmig0023l1", "ct", "ct", ""); err == nil {
		t.Error("empty label succeeded, want a CHECK violation")
	}
	if _, err := testPool.Exec(ctx, insert, "cmig0023l2", "ct", "ct", strings.Repeat("a", 201)); err == nil {
		t.Error("201-character label succeeded, want a CHECK violation")
	}
	// Exactly 200 is legal — the CHECK must not be off by one.
	if _, err := testPool.Exec(ctx, insert, "cmig0023l3", "ct", "ct", strings.Repeat("a", 200)); err != nil {
		t.Errorf("200-character label rejected: %v", err)
	}
}

// TestMigration0023_UpRequiresCoordinates proves the NOT NULL constraints: a
// place without coordinates is not a place, so the absent state is the absent
// ROW, never a row with NULL columns. This is what lets the wire envelope be a
// sparse array rather than a fixed pair with nullable members.
func TestMigration0023_UpRequiresCoordinates(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		latEnc any
		lngEnc any
	}{
		{"latitude null", nil, "ct-lng"},
		{"longitude null", "ct-lat", nil},
		{"both null", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testPool.Exec(ctx,
				`INSERT INTO go_saved_places (user_id, kind, lat_enc, lng_enc, label)
				 VALUES ($1,'home',$2,$3,'X')`,
				"cmig0023n_"+tc.name, tc.latEnc, tc.lngEnc)
			if err == nil {
				t.Fatal("insert with a NULL coordinate succeeded, want a NOT NULL violation")
			}
		})
	}
}

// TestMigration0023_DownDropsSavedPlacesOnly exercises the rollback and proves
// it is SURGICAL: go_saved_places goes and the Go-owned tables installed by
// earlier migrations survive.
//
// Migrates down TO VERSION 22 and back up, leaving the database at head for
// whatever runs next. Version-targeted rather than Steps(-1) on purpose: a
// relative step silently rolls back whatever the newest migration happens to
// be, so the next issue that adds one would turn this into a test of ITS
// down-migration.
func TestMigration0023_DownDropsSavedPlacesOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(22); err != nil {
		t.Fatalf("migrate down to 22: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if tableExists(t, "go_saved_places") {
		t.Error("go_saved_places survived the down-migration")
	}

	// The rollback must not take the rest of the Go-owned schema with it —
	// in particular the migration it sits directly on top of.
	for _, survivor := range []string{
		"go_ride_requests",  // MYR-173, migration 0002
		"go_users",          // MYR-193, migration 0003
		"go_push_devices",   // MYR-186, migration 0019
		"go_vehicle_shares", // MYR-184, migration 0020
		"go_push_prefs",     // MYR-349, migration 0022
	} {
		if !tableExists(t, survivor) {
			t.Errorf("table %s was dropped by the 0023 down-migration — it must be surgical", survivor)
		}
	}
}
