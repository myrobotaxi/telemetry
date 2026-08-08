package store_test

// MYR-447 acceptance test — the label half of the MYR-433 bar.
//
// MYR-433's executable acceptance test proved an operator with a psql
// prompt cannot read a user's COORDINATES. It did not prove they cannot
// read the ADDRESS those coordinates resolve to, and at the time they
// could: "locationAddress", "startAddress" and their four siblings sat in
// plaintext one column over from the sealed latitude. That is the more
// damaging half of the same disclosure — a coordinate needs a map, a
// street address needs nothing.
//
// This file is the executable form of the closing sentence. It writes a
// realistic user through the PRODUCTION write paths — a car reporting a
// geocoded current location and a navigation destination, and a drive
// that started at one address and ended at another — then runs the purge
// and queries the database exactly the way an operator with a psql prompt
// or a stolen dump would: plain SELECTs against the plaintext label
// columns, with no application code in between.
//
// Two things must hold at the end, and both are asserted:
//
//  1. Nothing legible. No place name and no street address is readable
//     from any plaintext label column.
//  2. Nothing lost. Every one of those labels is still recoverable
//     through the repositories, which hold the key. An empty database
//     would also pass (1); it must not pass (2).

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/plaintextpurge"
)

// labelFixture is the data the acceptance test plants and then tries to
// read back out of the raw tables.
//
// Every value carries an "MYR447-" marker because the assertions work by
// scanning whole column dumps for these needles: a plausible-looking
// address ("123 Market St") would hide in the noise of an ordinary row,
// and a bare "test" would collide with half the fixtures in the package.
type labelFixture struct {
	vin       string
	vehicleID string
	driveID   string

	locationName    string
	locationAddress string
	destName        string
	destAddress     string

	startLocation string
	startAddress  string
	endLocation   string
	endAddress    string
}

func newLabelFixture() labelFixture {
	return labelFixture{
		vin:       "5YJ3E1EA1NF447001",
		vehicleID: "veh_myr447_001",
		driveID:   "drv_myr447_001",

		locationName:    "MYR447-PLACE-Alcatraz-Landing",
		locationAddress: "MYR447-STREET-1600-Pennsylvania-Ave",
		destName:        "MYR447-PLACE-Coit-Tower",
		destAddress:     "MYR447-STREET-1-Telegraph-Hill-Blvd",

		startLocation: "MYR447-PLACE-Ferry-Building",
		startAddress:  "MYR447-STREET-1-Ferry-Building-Embarcadero",
		endLocation:   "MYR447-PLACE-Golden-Gate-Overlook",
		endAddress:    "MYR447-STREET-948-Fort-Baker-Rd",
	}
}

// needles returns every label an operator must NOT find in a plaintext
// column, keyed by a human-readable description for the failure message.
func (f labelFixture) needles() map[string]string {
	return map[string]string{
		"vehicle location place name": f.locationName,
		"vehicle location address":    f.locationAddress,
		"navigation destination name": f.destName,
		"navigation destination addr": f.destAddress,
		"drive start place name":      f.startLocation,
		"drive start street address":  f.startAddress,
		"drive end place name":        f.endLocation,
		"drive end street address":    f.endAddress,
	}
}

// TestMYR447_OperatorCannotReadLocationLabels is the acceptance test.
func TestMYR447_OperatorCannotReadLocationLabels(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)

	ctx := context.Background()
	enc := newTestEncryptor(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := newLabelFixture()

	seedVehicle(t, testPool, f.vehicleID, f.vin)
	writeLabelsThroughProductionPaths(ctx, t, enc, quiet, f)

	// Sanity-check the fixture BEFORE the purge. If the writes never
	// landed, every "not readable" assertion below would pass vacuously
	// and this test would be worthless.
	assertLabelsRecoverable(ctx, t, enc, quiet, f, "before purge")

	// The purge is the step that removes the pre-MYR-447 residue. In this
	// test the residue is planted deliberately, because the new write
	// paths no longer create any — which is itself the point.
	plantLegacyLabelResidue(ctx, t, f)
	runLabelPurgeToCompletion(ctx, t, enc, quiet)

	// The operator's view.
	assertNoLabelReadable(ctx, t, f)

	// And the data is still there for anyone holding the key.
	assertLabelsRecoverable(ctx, t, enc, quiet, f, "after purge")
}

// writeLabelsThroughProductionPaths persists the fixture using the same
// repositories the running server uses — not raw SQL. That matters: the
// claim under test is about what the production write paths leave behind,
// so hand-written INSERTs would prove nothing.
func writeLabelsThroughProductionPaths(
	ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger, f labelFixture,
) {
	t.Helper()

	// A car whose current position and navigation destination have both
	// been reverse-geocoded — the writer_location_address.go and
	// writer_destination_address.go outcomes, expressed as the
	// VehicleUpdate they hand to the repo.
	vehicles := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	if err := vehicles.UpdateTelemetry(ctx, f.vin, store.VehicleUpdate{
		LocationName:       &f.locationName,
		LocationAddr:       &f.locationAddress,
		DestinationName:    &f.destName,
		DestinationAddress: &f.destAddress,
		LastUpdated:        time.Now(),
	}); err != nil {
		t.Fatalf("update telemetry: %v", err)
	}

	// A drive that started at one geocoded address (DriveRepo.Create, the
	// handleDriveStarted path) and ended at another (DriveRepo.Complete,
	// the handleDriveEnded path). Both sides matter: they are different
	// statements writing different columns.
	drives := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	if err := drives.Create(ctx, store.DriveRecord{
		ID:            f.driveID,
		VehicleID:     f.vehicleID,
		Date:          "2026-08-07",
		StartTime:     "2026-08-07T10:00:00Z",
		StartLocation: f.startLocation,
		StartAddress:  f.startAddress,
	}); err != nil {
		t.Fatalf("create drive: %v", err)
	}
	if err := drives.Complete(ctx, f.driveID, store.DriveCompletion{
		EndTime:     "2026-08-07T10:42:00Z",
		EndLocation: f.endLocation,
		EndAddress:  f.endAddress,
	}); err != nil {
		t.Fatalf("complete drive: %v", err)
	}
}

// plantLegacyLabelResidue writes the fixture's labels into the plaintext
// columns by hand, simulating a database that has been running since
// before MYR-447.
//
// Without this the test could not tell a working purge apart from a purge
// that does nothing, because the new write paths leave those columns
// empty already. Planting the residue is what makes the post-purge
// assertions meaningful.
func plantLegacyLabelResidue(ctx context.Context, t *testing.T, f labelFixture) {
	t.Helper()

	if _, err := testPool.Exec(ctx, `
		UPDATE "Vehicle" SET
			"locationName" = $2, "locationAddress" = $3,
			"destinationName" = $4, "destinationAddress" = $5
		WHERE "vin" = $1`,
		f.vin, f.locationName, f.locationAddress, f.destName, f.destAddress,
	); err != nil {
		t.Fatalf("plant vehicle label residue: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		UPDATE "Drive" SET
			"startLocation" = $2, "startAddress" = $3,
			"endLocation" = $4, "endAddress" = $5
		WHERE "id" = $1`,
		f.driveID, f.startLocation, f.startAddress, f.endLocation, f.endAddress,
	); err != nil {
		t.Fatalf("plant drive label residue: %v", err)
	}
}

// runLabelPurgeToCompletion runs the purge and requires a clean sweep of
// the eight label targets: nothing blocked, nothing left behind.
//
// It asserts on the label targets specifically rather than on the global
// totals, because this test seeds no Account row and the purge's token
// targets would otherwise dominate the numbers.
func runLabelPurgeToCompletion(ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger) {
	t.Helper()

	// Dry run first — it must find the same work and change nothing.
	dry, err := plaintextpurge.New(testPool, enc, logger).Run(ctx, true)
	if err != nil {
		t.Fatalf("dry-run purge: %v", err)
	}
	if dry.TotalPurged() != 0 {
		t.Errorf("dry run purged %d rows; a dry run must not write", dry.TotalPurged())
	}
	if labelRemaining(dry) == 0 {
		t.Fatal("dry run found no label residue to purge; the planted residue is missing")
	}

	res, err := plaintextpurge.New(testPool, enc, logger).Run(ctx, false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, label := range labelTargetLabels() {
		tr, ok := res.Targets[label]
		if !ok {
			t.Errorf("purge has no target for %s; its plaintext would never be scrubbed", label)
			continue
		}
		if tr.Blocked() != 0 {
			t.Errorf("%s: purge left %d row(s) unverifiable; every planted label was sealed "+
				"by the production write path and should have verified", label, tr.Blocked())
		}
		if tr.UpdateErrors != 0 {
			t.Errorf("%s: purge hit %d scrub-write error(s)", label, tr.UpdateErrors)
		}
		if tr.Remaining != 0 {
			t.Errorf("%s: purge finished with %d readable plaintext row(s), want 0", label, tr.Remaining)
		}
	}

	// Idempotence: a second run must find nothing left in the label
	// targets.
	again, err := plaintextpurge.New(testPool, enc, logger).Run(ctx, false)
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	for _, label := range labelTargetLabels() {
		if tr := again.Targets[label]; tr.Purged != 0 {
			t.Errorf("%s: second purge scrubbed %d row(s); the purge is not idempotent", label, tr.Purged)
		}
	}
}

// labelTargetLabels is the eight `<table>.<name>` keys MYR-447 added to
// plaintextpurge.Targets. Spelled out here rather than derived from the
// package so a target silently renamed or dropped fails this test.
func labelTargetLabels() []string {
	return []string{
		"Vehicle.locationName", "Vehicle.locationAddress",
		"Vehicle.destinationName", "Vehicle.destinationAddress",
		"Drive.startLocation", "Drive.startAddress",
		"Drive.endLocation", "Drive.endAddress",
	}
}

// labelRemaining sums the residual plaintext rows across the eight label
// targets only.
func labelRemaining(r plaintextpurge.Result) int {
	n := 0
	for _, label := range labelTargetLabels() {
		n += r.Targets[label].Remaining
	}
	return n
}

// assertNoLabelReadable is the operator's view: raw SELECTs against the
// plaintext label columns, no application code in the path.
func assertNoLabelReadable(ctx context.Context, t *testing.T, f labelFixture) {
	t.Helper()

	t.Run("vehicle location and destination labels", func(t *testing.T) {
		var locName, locAddr string
		var destName, destAddr *string
		if err := testPool.QueryRow(ctx, `
			SELECT "locationName", "locationAddress", "destinationName", "destinationAddress"
			FROM "Vehicle" WHERE "vin" = $1`, f.vin,
		).Scan(&locName, &locAddr, &destName, &destAddr); err != nil {
			t.Fatalf("operator SELECT on Vehicle: %v", err)
		}
		// locationName / locationAddress are NOT NULL on the Prisma
		// schema, so the scrubbed value is the empty string, not NULL.
		if locName != "" || locAddr != "" {
			t.Errorf("operator can read where this car is: name=%q address=%q; want empty strings",
				locName, locAddr)
		}
		for name, v := range map[string]*string{
			"destinationName": destName, "destinationAddress": destAddr,
		} {
			if v != nil {
				t.Errorf("operator can read %s = %q; want NULL — that is where the user is going", name, *v)
			}
		}
	})

	t.Run("drive endpoint labels", func(t *testing.T) {
		var startLoc, startAddr, endLoc, endAddr string
		if err := testPool.QueryRow(ctx, `
			SELECT "startLocation", "startAddress", "endLocation", "endAddress"
			FROM "Drive" WHERE "id" = $1`, f.driveID,
		).Scan(&startLoc, &startAddr, &endLoc, &endAddr); err != nil {
			t.Fatalf("operator SELECT on Drive: %v", err)
		}
		// All four are NOT NULL, so the scrubbed value is the empty string.
		for name, v := range map[string]string{
			"startLocation": startLoc, "startAddress": startAddr,
			"endLocation": endLoc, "endAddress": endAddr,
		} {
			if v != "" {
				t.Errorf("operator can read Drive.%s = %q; want the empty string", name, v)
			}
		}
	})

	// The broad sweep. The per-column assertions above only look where we
	// already know to look; this dumps every plaintext label column across
	// both tables into one string and hunts for the fixture's needles.
	t.Run("no label appears anywhere in the plaintext columns", func(t *testing.T) {
		dump := dumpPlaintextLabelColumns(ctx, t)
		for label, needle := range f.needles() {
			if strings.Contains(dump, needle) {
				t.Errorf("operator can still read the %s (%q) in a plaintext column", label, needle)
			}
		}
	})

	// And the ciphertext really is ciphertext, not an encoding mistake
	// that left readable text behind.
	t.Run("label ciphertext columns are opaque", func(t *testing.T) {
		assertLabelCiphertextOpaque(ctx, t, f)
	})
}

// assertLabelCiphertextOpaque proves the `*Enc` columns hold real
// AES-256-GCM output: base64-decodable, long enough to carry the wire
// framing, and free of the plaintext they were built from.
func assertLabelCiphertextOpaque(ctx context.Context, t *testing.T, f labelFixture) {
	t.Helper()

	got := map[string]string{}
	var locName, locAddr, destName, destAddr string
	if err := testPool.QueryRow(ctx, `
		SELECT "locationNameEnc", "locationAddressEnc",
		       "destinationNameEnc", "destinationAddressEnc"
		FROM "Vehicle" WHERE "vin" = $1`, f.vin,
	).Scan(&locName, &locAddr, &destName, &destAddr); err != nil {
		t.Fatalf("read vehicle label ciphertext: %v", err)
	}
	got["locationNameEnc"] = locName
	got["locationAddressEnc"] = locAddr
	got["destinationNameEnc"] = destName
	got["destinationAddressEnc"] = destAddr

	var startLoc, startAddr, endLoc, endAddr string
	if err := testPool.QueryRow(ctx, `
		SELECT "startLocationEnc", "startAddressEnc", "endLocationEnc", "endAddressEnc"
		FROM "Drive" WHERE "id" = $1`, f.driveID,
	).Scan(&startLoc, &startAddr, &endLoc, &endAddr); err != nil {
		t.Fatalf("read drive label ciphertext: %v", err)
	}
	got["startLocationEnc"] = startLoc
	got["startAddressEnc"] = startAddr
	got["endLocationEnc"] = endLoc
	got["endAddressEnc"] = endAddr

	needles := f.needles()
	for name, ct := range got {
		if ct == "" {
			t.Errorf("%s is empty — the label was not sealed at all", name)
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(ct)
		if err != nil {
			t.Errorf("%s is not base64: %v", name, err)
			continue
		}
		// version byte + 12-byte nonce + 16-byte GCM tag.
		if len(raw) < 1+12+16 {
			t.Errorf("%s is %d bytes, too short to be AES-GCM output", name, len(raw))
		}
		for _, needle := range needles {
			if strings.Contains(ct, needle) {
				t.Errorf("%s contains its own plaintext (%q)", name, needle)
			}
		}
	}
}

// dumpPlaintextLabelColumns concatenates every retired plaintext label
// column across both tables into one blob, so a single scan can prove a
// label appears in none of them.
func dumpPlaintextLabelColumns(ctx context.Context, t *testing.T) string {
	t.Helper()
	var b strings.Builder

	for _, q := range []string{
		`SELECT COALESCE("locationName",'') || '|' || COALESCE("locationAddress",'') || '|' ||
		        COALESCE("destinationName",'') || '|' || COALESCE("destinationAddress",'')
		 FROM "Vehicle"`,
		`SELECT COALESCE("startLocation",'') || '|' || COALESCE("startAddress",'') || '|' ||
		        COALESCE("endLocation",'') || '|' || COALESCE("endAddress",'')
		 FROM "Drive"`,
	} {
		rows, err := testPool.Query(ctx, q)
		if err != nil {
			t.Fatalf("dump plaintext label columns: %v", err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatalf("scan plaintext label dump: %v", err)
			}
			b.WriteString(s)
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate plaintext label dump: %v", err)
		}
		rows.Close()
	}
	return b.String()
}

// assertLabelsRecoverable proves the other half of the bar: the labels
// are encrypted, not destroyed. Anyone holding the key still reads
// exactly what was written — through every wire surface that carries a
// label, so a regression on any one of them fails here.
func assertLabelsRecoverable(
	ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger,
	f labelFixture, when string,
) {
	t.Helper()

	vehicles := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	drives := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)

	// GET /api/vehicles/{id}/snapshot and the WS vehicle_update replay
	// frame both read through GetByID; GetByVIN is the telemetry-side
	// read. Assert both so a projection/scan-order mistake on either is
	// caught.
	for _, tc := range []struct {
		name string
		get  func() (store.Vehicle, error)
	}{
		{"GetByVIN", func() (store.Vehicle, error) { return vehicles.GetByVIN(ctx, f.vin) }},
		{"GetByID", func() (store.Vehicle, error) { return vehicles.GetByID(ctx, f.vehicleID) }},
	} {
		v, err := tc.get()
		if err != nil {
			t.Fatalf("%s: %s: %v", when, tc.name, err)
		}
		if v.LocationName != f.locationName {
			t.Errorf("%s: %s: locationName = %q, want %q", when, tc.name, v.LocationName, f.locationName)
		}
		if v.LocationAddress != f.locationAddress {
			t.Errorf("%s: %s: locationAddress = %q, want %q", when, tc.name, v.LocationAddress, f.locationAddress)
		}
		if v.DestinationName == nil || *v.DestinationName != f.destName {
			t.Errorf("%s: %s: destinationName not recoverable: %v", when, tc.name, v.DestinationName)
		}
		if v.DestinationAddress == nil || *v.DestinationAddress != f.destAddress {
			t.Errorf("%s: %s: destinationAddress not recoverable: %v", when, tc.name, v.DestinationAddress)
		}
	}

	// GET /api/drives/{id}.
	d, err := drives.GetByID(ctx, f.driveID)
	if err != nil {
		t.Fatalf("%s: DriveRepo.GetByID: %v", when, err)
	}
	assertDriveLabels(t, when, "GetByID",
		d.StartLocation, d.StartAddress, d.EndLocation, d.EndAddress, f)

	// GET /api/vehicles/{id}/drives — the drives feed. Its lean
	// projection is a separate SELECT and a separate scan, so it can
	// regress independently of the detail read.
	page, err := drives.ListByVehicleID(ctx, f.vehicleID, store.DriveListCursor{}, 10)
	if err != nil {
		t.Fatalf("%s: ListByVehicleID: %v", when, err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("%s: drives feed returned %d rows, want 1", when, len(page.Items))
	}
	row := page.Items[0]
	assertDriveLabels(t, when, "drives feed",
		row.StartLocation, row.StartAddress, row.EndLocation, row.EndAddress, f)
}

// assertDriveLabels compares one read path's four drive labels against
// the fixture. Shared by the detail read and the list read so the two
// cannot drift.
func assertDriveLabels(
	t *testing.T, when, path, startLoc, startAddr, endLoc, endAddr string, f labelFixture,
) {
	t.Helper()
	for _, c := range []struct {
		field string
		got   string
		want  string
	}{
		{"startLocation", startLoc, f.startLocation},
		{"startAddress", startAddr, f.startAddress},
		{"endLocation", endLoc, f.endLocation},
		{"endAddress", endAddr, f.endAddress},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s: %s = %q, want %q", when, path, c.field, c.got, c.want)
		}
	}
}
