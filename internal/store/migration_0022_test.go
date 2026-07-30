package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-349 migration 0022: the go_push_prefs table.
//
// Like the 0021 test, the assertions are about NULLABILITY and DEFAULT rather
// than mere presence, because those two ARE the design. Every column is NOT
// NULL DEFAULT TRUE, and each half fails in its own bad direction:
//
//   - drop the DEFAULT and a row inserted without the column fails outright,
//     taking the §7.19 write with it — and worse, the "no row" and "row with
//     the column defaulted" states stop agreeing;
//   - drop NOT NULL and a NULL becomes reachable, which Go's Scan into a bool
//     refuses — so the READ path breaks for exactly the accounts that have a
//     row, i.e. the people who bothered to set a preference.

// migration0022ColumnState is a column's declared shape.
type migration0022ColumnState struct {
	dataType string
	nullable string
	def      string
}

// pushPrefsColumnState reads the live type, nullability and default for one
// go_push_prefs column.
func pushPrefsColumnState(t *testing.T, column string) (migration0022ColumnState, bool) {
	t.Helper()
	var got migration0022ColumnState
	var def *string
	err := testPool.QueryRow(context.Background(),
		`SELECT data_type, is_nullable, column_default
		 FROM information_schema.columns
		 WHERE table_name = 'go_push_prefs'
		   AND column_name = $1`, column).Scan(&got.dataType, &got.nullable, &def)
	if err != nil {
		return migration0022ColumnState{}, false
	}
	if def != nil {
		got.def = *def
	}
	return got, true
}

// pushPrefsCategoryColumns is every category column. Kept as a literal rather
// than derived from the Go type, so that adding a column to one without the
// other is a test failure instead of a silent gap.
var pushPrefsCategoryColumns = []string{
	"ride_lifecycle",
	"drive_started",
	"drive_completed",
	"charging_complete",
	"viewer_joined",
}

// TestMigration0022_CreatesPushPrefsTable verifies every category column lands
// as a NOT NULL boolean defaulting to true.
func TestMigration0022_CreatesPushPrefsTable(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	for _, column := range pushPrefsCategoryColumns {
		t.Run(column, func(t *testing.T) {
			got, ok := pushPrefsColumnState(t, column)
			if !ok {
				t.Fatalf("column %s missing after migrate up", column)
			}
			if got.dataType != "boolean" {
				t.Errorf("data_type = %q, want %q", got.dataType, "boolean")
			}
			if got.nullable != "NO" {
				t.Errorf("is_nullable = %q, want NO — there is no unknown state for a "+
					"delivery preference, and a NULL would break the read path for "+
					"exactly the accounts that set one", got.nullable)
			}
			if got.def != "true" {
				t.Errorf("column_default = %q, want %q — the default is what makes "+
					"'no row' and 'row never touched' indistinguishable to the notifier",
					got.def, "true")
			}
		})
	}
}

// TestMigration0022_UserIDIsThePrimaryKey pins the key choice. A surrogate id
// would allow a second row per person, which would silently shadow the first —
// and the shadowed row is the one somebody's switches are in.
func TestMigration0022_UserIDIsThePrimaryKey(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var columns []string
	rows, err := testPool.Query(context.Background(),
		`SELECT a.attname
		 FROM pg_index i
		 JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		 WHERE i.indrelid = 'go_push_prefs'::regclass AND i.indisprimary`)
	if err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if len(columns) != 1 || columns[0] != "user_id" {
		t.Errorf("primary key = %v, want exactly [user_id] — a second row per person "+
			"would shadow the one holding their real switches", columns)
	}
}

// TestMigration0022_RowInsertedWithNoCategoriesIsAllOn proves the DEFAULT is
// actually applied, rather than merely declared.
func TestMigration0022_RowInsertedWithNoCategoriesIsAllOn(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	cleanPushPrefs(t)

	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_push_prefs (user_id) VALUES ($1)`, "cuser-bare-insert"); err != nil {
		t.Fatalf("bare insert: %v — every category column must be defaultable", err)
	}

	prefs, err := store.NewPushPrefsRepo(testPool, testLogger()).
		PrefsForUser(ctx, "cuser-bare-insert")
	if err != nil {
		t.Fatalf("PrefsForUser: %v", err)
	}
	if prefs != store.DefaultPushPrefs() {
		t.Errorf("bare-inserted row reads %+v, want %+v", prefs, store.DefaultPushPrefs())
	}
}

// TestMigration0022_NoForeignKeys pins CG-DL-9 for this table: no FK may point
// at anything the sibling app's ORM owns.
func TestMigration0022_NoForeignKeys(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM pg_constraint
		 WHERE conrelid = 'go_push_prefs'::regclass AND contype = 'f'`).Scan(&count); err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	if count != 0 {
		t.Errorf("go_push_prefs declares %d foreign key(s), want 0 (CG-DL-9)", count)
	}
}
