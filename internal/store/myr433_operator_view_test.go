package store_test

// MYR-433 acceptance test. The client's bar, verbatim:
//
//	"I should not be able to go into the db and see users drive or if
//	 there's a db leak it should be encrypted."
//
// This file is the executable form of that sentence. It writes a
// realistic user through the PRODUCTION write paths — a linked Tesla
// account, a car reporting GPS and a planned nav route, and a drive
// recording a GPS trail — then runs the purge and queries the database
// exactly the way an operator with a psql prompt or a stolen dump would:
// plain SELECTs against the plaintext columns, with no application code
// in between.
//
// Two things must hold at the end, and both are asserted:
//
//  1. Nothing legible. No coordinate, no route geometry, no Tesla token
//     is readable from any plaintext column.
//  2. Nothing lost. Every one of those values is still recoverable
//     through the repositories, which hold the key. An empty database
//     would also pass (1); it must not pass (2).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/plaintextpurge"
)

// operatorFixture is the data the acceptance test plants and then tries
// to read back out of the raw tables.
type operatorFixture struct {
	vin          string
	vehicleID    string
	driveID      string
	userID       string
	lat, lng     float64
	destLat      float64
	destLng      float64
	originLat    float64
	originLng    float64
	navRoute     string
	trail        []store.RoutePointRecord
	accessToken  string
	refreshToken string
	idToken      string
}

// newOperatorFixture returns the fixture with deliberately distinctive
// values.
//
// The coordinates are real and specific, and the tokens carry a
// recognisable marker, because the assertions work by scanning whole
// column dumps for these needles. A value like 0 or "test" would hide in
// the noise of an ordinary row.
func newOperatorFixture() operatorFixture {
	return operatorFixture{
		vin:       "5YJ3E1EA1NF433001",
		vehicleID: "veh_myr433_001",
		driveID:   "drv_myr433_001",
		userID:    "user_001",
		lat:       33.0975241,
		lng:       -96.8214773,
		destLat:   32.7811119,
		destLng:   -96.8021551,
		originLat: 33.1005992,
		originLng: -96.8299001,
		navRoute:  `[[-96.8214773,33.0975241],[-96.8100011,33.0900022],[-96.8021551,32.7811119]]`,
		trail: []store.RoutePointRecord{
			{Latitude: 33.0975241, Longitude: -96.8214773, Speed: 31, Heading: 180, Timestamp: "2026-08-02T10:00:00Z"},
			{Latitude: 33.0900022, Longitude: -96.8100011, Speed: 44, Heading: 178, Timestamp: "2026-08-02T10:01:00Z"},
			{Latitude: 32.7811119, Longitude: -96.8021551, Speed: 0, Heading: 176, Timestamp: "2026-08-02T10:24:00Z"},
		},
		// #nosec G101 -- fabricated test fixtures, not real credentials. The
		// marker prefix is what the needle scan below greps for.
		accessToken:  "qts-MYR433-ACCESS-a1b2c3d4e5f6",
		refreshToken: "qts-MYR433-REFRESH-9z8y7x6w5v",
		idToken:      "qts-MYR433-IDTOKEN-4k3j2h1g0f",
	}
}

// needles returns every secret string an operator must NOT find. Floats
// are rendered the way Postgres renders a double precision column so a
// substring scan over a ::text dump will actually catch them.
func (f operatorFixture) needles() map[string]string {
	return map[string]string{
		"vehicle latitude":      fmt.Sprintf("%v", f.lat),
		"vehicle longitude":     fmt.Sprintf("%v", f.lng),
		"destination latitude":  fmt.Sprintf("%v", f.destLat),
		"destination longitude": fmt.Sprintf("%v", f.destLng),
		"origin latitude":       fmt.Sprintf("%v", f.originLat),
		"origin longitude":      fmt.Sprintf("%v", f.originLng),
		"tesla access token":    f.accessToken,
		"tesla refresh token":   f.refreshToken,
		"tesla id token":        f.idToken,
	}
}

// TestMYR433_OperatorCannotReadPlaintext is the acceptance test.
func TestMYR433_OperatorCannotReadPlaintext(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	// ensureOwnerSchema brings up the Prisma-owned "Account"/"Settings"
	// tables the provisioner writes to; the base harness only creates
	// User/Vehicle/Drive.
	ensureOwnerSchema(t)
	cleanTables(t, testPool)
	cleanOwnerTables(t)

	ctx := context.Background()
	enc := newTestEncryptor(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := newOperatorFixture()

	seedOwnerUser(t, f.userID, "MYR-433 Owner", "myr433@example.com")
	seedVehicle(t, testPool, f.vehicleID, f.vin)
	writeFixtureThroughProductionPaths(ctx, t, enc, quiet, f)

	// Sanity-check the fixture BEFORE the purge. If the writes never
	// landed, every "not readable" assertion below would pass vacuously
	// and this test would be worthless.
	assertRecoverableThroughRepos(ctx, t, enc, quiet, f, "before purge")

	// The purge is the step that removes the pre-MYR-433 residue. In this
	// test the residue is planted deliberately (see
	// plantLegacyPlaintextResidue) because the new write paths no longer
	// create any — which is itself the point.
	plantLegacyPlaintextResidue(ctx, t, f, enc)
	runPurgeToCompletion(ctx, t, enc, quiet)

	// The operator's view.
	assertNoPlaintextReadable(ctx, t, f)

	// And the data is still there for anyone holding the key.
	assertRecoverableThroughRepos(ctx, t, enc, quiet, f, "after purge")
}

// TestMYR433_PurgeScrubsPostDeploySkew is the regression test for the
// bug that made the first cut of this work useless on a live database.
//
// The original purge scrubbed only when the decrypted ciphertext EQUALLED
// the plaintext. That looks careful and is catastrophic: once the
// ciphertext-only server is deployed, plaintext stops being written while
// ciphertext keeps advancing on every telemetry frame, token refresh and
// route flush. So every ACTIVE row drifts apart within seconds and would
// have been refused forever — leaving live Tesla credentials and live
// coordinates readable on exactly the accounts that matter most, with
// totalRemaining permanently non-zero. Re-running could not help, because
// the backfills only populate a NULL ciphertext.
//
// The acceptance test above cannot catch this: it writes and purges in
// one pass, so its two copies never diverge. This one deliberately
// advances the ciphertext AFTER planting the legacy plaintext, exactly as
// a running server would, and asserts the purge still clears the row.
func TestMYR433_PurgeScrubsPostDeploySkew(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	ensureOwnerSchema(t)
	cleanTables(t, testPool)
	cleanOwnerTables(t)

	ctx := context.Background()
	enc := newTestEncryptor(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := newOperatorFixture()

	seedOwnerUser(t, f.userID, "MYR-433 Owner", "myr433@example.com")
	seedVehicle(t, testPool, f.vehicleID, f.vin)
	writeFixtureThroughProductionPaths(ctx, t, enc, quiet, f)
	plantLegacyPlaintextResidue(ctx, t, f, enc)

	// Now let the "server" run: every write below touches ONLY ciphertext,
	// which is what the post-MYR-433 write paths do. The plaintext columns
	// keep their planted pre-deploy values and now disagree with it.
	advanceCiphertextOnly(ctx, t, enc, quiet, f)

	res, err := plaintextpurge.New(testPool, enc, quiet).Run(ctx, false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}

	if res.TotalStale() == 0 {
		t.Error("no rows were classified stale; the skew this test exists to create did not happen, " +
			"so it is not exercising the regression")
	}
	if res.TotalBlocked() != 0 {
		t.Errorf("purge blocked %d row(s) after a normal post-deploy skew. This is the MYR-433 "+
			"blocker: a ciphertext that advanced past its plaintext is STALE, not suspect, and "+
			"must be scrubbed. Per-target: %+v", res.TotalBlocked(), res.Targets)
	}
	if res.TotalRemaining() != 0 {
		t.Errorf("purge left %d readable plaintext row(s) on a live-traffic database; "+
			"totalRemaining must reach 0 or the acceptance bar is unreachable. Per-target: %+v",
			res.TotalRemaining(), res.Targets)
	}

	// And the operator still cannot read anything.
	assertNoPlaintextReadable(ctx, t, f)
}

// advanceCiphertextOnly simulates live traffic after the ciphertext-only
// server is deployed: new GPS, a new route, more trail points and a token
// refresh, all of which land in the *Enc columns and none of which touch
// the plaintext columns.
func advanceCiphertextOnly(
	ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger, f operatorFixture,
) {
	t.Helper()

	newLat, newLng := 30.2672012, -97.7430613 // the car drove to Austin
	newDestLat, newDestLng := 30.1975001, -97.6664002
	newOriginLat, newOriginLng := 30.3000003, -97.7500004
	newRoute := json.RawMessage(`[[-97.7430613,30.2672012],[-97.6664002,30.1975001]]`)

	vehicles := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	if err := vehicles.UpdateTelemetry(ctx, f.vin, store.VehicleUpdate{
		Latitude:             &newLat,
		Longitude:            &newLng,
		DestinationLatitude:  &newDestLat,
		DestinationLongitude: &newDestLng,
		OriginLatitude:       &newOriginLat,
		OriginLongitude:      &newOriginLng,
		NavRouteCoordinates:  &newRoute,
		LastUpdated:          time.Now(),
	}); err != nil {
		t.Fatalf("advance vehicle telemetry: %v", err)
	}

	drives := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	if err := drives.AppendRoutePoints(ctx, f.driveID, []store.RoutePointRecord{
		{Latitude: 30.2672012, Longitude: -97.7430613, Speed: 55, Heading: 90, Timestamp: "2026-08-02T11:00:00Z"},
	}); err != nil {
		t.Fatalf("advance drive trail: %v", err)
	}

	accounts := store.NewAccountRepo(testPool, enc)
	if err := accounts.UpdateTeslaToken(ctx, f.userID,
		"qts-MYR433-ACCESS-REFRESHED", "qts-MYR433-REFRESH-ROTATED",
		time.Now().Add(8*time.Hour).Unix(),
	); err != nil {
		t.Fatalf("advance tesla token: %v", err)
	}

	// Guard the premise: the vehicle's plaintext columns must still hold
	// their OLD values, or the writes above touched plaintext and this
	// test proves nothing about skew.
	var lat float64
	if err := testPool.QueryRow(ctx,
		`SELECT "latitude" FROM "Vehicle" WHERE "vin" = $1`, f.vin).Scan(&lat); err != nil {
		t.Fatalf("read plaintext latitude: %v", err)
	}
	if lat != f.lat {
		t.Fatalf("plaintext latitude moved to %v; a write path is still touching the plaintext column", lat)
	}

	// The Account row is the deliberate exception. UpdateTeslaToken NULLs
	// the plaintext token columns as it writes (queries.go), so a refresh
	// heals the row on its own rather than leaving a superseded credential
	// readable until someone runs the purge. Assert that self-healing here
	// — it is the one plaintext write this server still makes, and it only
	// ever writes NULL.
	var access, refresh *string
	if err := testPool.QueryRow(ctx,
		`SELECT "access_token", "refresh_token" FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla'`,
		f.userID).Scan(&access, &refresh); err != nil {
		t.Fatalf("read plaintext tokens: %v", err)
	}
	if access != nil || refresh != nil {
		t.Errorf("token refresh left plaintext credentials behind (access=%v refresh=%v); "+
			"UpdateTeslaToken is supposed to scrub them", drefOrNil(access), drefOrNil(refresh))
	}
}

// drefOrNil renders a nullable string column for an error message.
func drefOrNil(p *string) string {
	if p == nil {
		return "NULL"
	}
	return *p
}

// writeFixtureThroughProductionPaths persists the fixture using the same
// repositories the running server uses — not raw SQL. That matters: the
// claim under test is about what the production write paths leave behind,
// so hand-written INSERTs would prove nothing.
func writeFixtureThroughProductionPaths(
	ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger, f operatorFixture,
) {
	t.Helper()

	// A linked Tesla account, via the owner provisioner.
	prov := store.NewOwnerProvisioner(testPool, enc, logger)
	if _, err := prov.ProvisionTeslaOwner(ctx, store.ProvisionInput{
		UserID:            f.userID,
		ProviderAccountID: "tesla-myr433-001",
		Name:              "MYR-433 Owner",
		Email:             "myr433@example.com",
		AccessToken:       f.accessToken,
		RefreshToken:      f.refreshToken,
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("provision tesla owner: %v", err)
	}

	// A car reporting position, destination, origin and a planned route.
	vehicles := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	navRoute := json.RawMessage(f.navRoute)
	if err := vehicles.UpdateTelemetry(ctx, f.vin, store.VehicleUpdate{
		Latitude:             &f.lat,
		Longitude:            &f.lng,
		DestinationLatitude:  &f.destLat,
		DestinationLongitude: &f.destLng,
		OriginLatitude:       &f.originLat,
		OriginLongitude:      &f.originLng,
		NavRouteCoordinates:  &navRoute,
		LastUpdated:          time.Now(),
	}); err != nil {
		t.Fatalf("update telemetry: %v", err)
	}

	// A drive that recorded a GPS trail. Appended in two calls so the
	// transactional decrypt-append-reseal cycle is exercised, not just a
	// single seed write.
	drives := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	if err := drives.Create(ctx, store.DriveRecord{
		ID:        f.driveID,
		VehicleID: f.vehicleID,
		Date:      "2026-08-02",
		StartTime: "2026-08-02T10:00:00Z",
	}); err != nil {
		t.Fatalf("create drive: %v", err)
	}
	if err := drives.AppendRoutePoints(ctx, f.driveID, f.trail[:1]); err != nil {
		t.Fatalf("append first route point: %v", err)
	}
	if err := drives.AppendRoutePoints(ctx, f.driveID, f.trail[1:]); err != nil {
		t.Fatalf("append remaining route points: %v", err)
	}
}

// plantLegacyPlaintextResidue writes the fixture's secrets into the
// plaintext columns by hand, simulating a database that has been running
// since before MYR-433.
//
// Without this the test could not tell a working purge apart from a purge
// that does nothing, because the new write paths leave those columns
// empty already. Planting the residue is what makes the post-purge
// assertions meaningful.
//
// The id_token pair is planted wholesale — plaintext AND ciphertext.
// Nothing in this server has ever written id_token (the Tesla refresh
// response does not return one), so its ciphertext can only come from
// cmd/backfill-account-tokens. Sealing it here is what stops that column
// from being covered vacuously: without a row, "id_token is NULL" would
// pass whether or not the purge handles the column at all.
func plantLegacyPlaintextResidue(ctx context.Context, t *testing.T, f operatorFixture, enc cryptox.Encryptor) {
	t.Helper()

	idEnc, err := enc.EncryptString(f.idToken)
	if err != nil {
		t.Fatalf("seal id_token: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE "Account" SET "id_token" = $2, "id_token_enc" = $3
		WHERE "userId" = $1 AND "provider" = 'tesla'`,
		f.userID, f.idToken, idEnc,
	); err != nil {
		t.Fatalf("plant id_token pair: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		UPDATE "Vehicle" SET
			"latitude" = $2, "longitude" = $3,
			"destinationLatitude" = $4, "destinationLongitude" = $5,
			"originLatitude" = $6, "originLongitude" = $7,
			"navRouteCoordinates" = $8::jsonb
		WHERE "vin" = $1`,
		f.vin, f.lat, f.lng, f.destLat, f.destLng, f.originLat, f.originLng, f.navRoute,
	); err != nil {
		t.Fatalf("plant vehicle residue: %v", err)
	}

	trailJSON, mErr := json.Marshal(f.trail)
	if mErr != nil {
		t.Fatalf("marshal trail: %v", mErr)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE "Drive" SET "routePoints" = $2::jsonb WHERE "id" = $1`,
		f.driveID, json.RawMessage(trailJSON),
	); err != nil {
		t.Fatalf("plant drive residue: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		UPDATE "Account" SET "access_token" = $2, "refresh_token" = $3
		WHERE "userId" = $1 AND "provider" = 'tesla'`,
		f.userID, f.accessToken, f.refreshToken,
	); err != nil {
		t.Fatalf("plant account residue: %v", err)
	}
}

// runPurgeToCompletion runs the purge and requires a clean sweep: nothing
// blocked, nothing left behind.
func runPurgeToCompletion(ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger) {
	t.Helper()

	// Dry run first — it must find the same work and change nothing.
	dry, err := plaintextpurge.New(testPool, enc, logger).Run(ctx, true)
	if err != nil {
		t.Fatalf("dry-run purge: %v", err)
	}
	if dry.TotalPurged() != 0 {
		t.Errorf("dry run purged %d rows; a dry run must not write", dry.TotalPurged())
	}
	if dry.TotalRemaining() == 0 {
		t.Fatal("dry run found nothing to purge; the planted residue is missing")
	}

	res, err := plaintextpurge.New(testPool, enc, logger).Run(ctx, false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.TotalBlocked() != 0 {
		t.Errorf("purge left %d row(s) unverifiable; every planted row was sealed by the "+
			"production write path and should have verified", res.TotalBlocked())
	}
	if res.UpdateErrors() != 0 {
		t.Errorf("purge hit %d scrub-write error(s)", res.UpdateErrors())
	}
	if res.TotalRemaining() != 0 {
		t.Errorf("purge finished with %d plaintext row(s) remaining, want 0; per-column: %+v",
			res.TotalRemaining(), res.Targets)
	}

	// Idempotence: a second run must find nothing.
	again, err := plaintextpurge.New(testPool, enc, logger).Run(ctx, false)
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if again.TotalPurged() != 0 {
		t.Errorf("second purge scrubbed %d row(s); the purge is not idempotent", again.TotalPurged())
	}
}

// assertNoPlaintextReadable is the operator's view: raw SELECTs against
// the plaintext columns, no application code in the path.
func assertNoPlaintextReadable(ctx context.Context, t *testing.T, f operatorFixture) {
	t.Helper()

	t.Run("vehicle GPS and nav route", func(t *testing.T) {
		var lat, lng float64
		var destLat, destLng, originLat, originLng *float64
		var navRoute *string
		err := testPool.QueryRow(ctx, `
			SELECT "latitude", "longitude",
			       "destinationLatitude", "destinationLongitude",
			       "originLatitude", "originLongitude",
			       "navRouteCoordinates"::text
			FROM "Vehicle" WHERE "vin" = $1`, f.vin,
		).Scan(&lat, &lng, &destLat, &destLng, &originLat, &originLng, &navRoute)
		if err != nil {
			t.Fatalf("operator SELECT on Vehicle: %v", err)
		}

		// latitude/longitude are NOT NULL on the Prisma schema, so the
		// scrubbed value is the zero coordinate rather than NULL.
		if lat != 0 || lng != 0 {
			t.Errorf("operator can read vehicle position (%v, %v); want the zero coordinate", lat, lng)
		}
		for name, v := range map[string]*float64{
			"destinationLatitude":  destLat,
			"destinationLongitude": destLng,
			"originLatitude":       originLat,
			"originLongitude":      originLng,
		} {
			if v != nil {
				t.Errorf("operator can read %s = %v; want NULL", name, *v)
			}
		}
		if navRoute != nil {
			t.Errorf("operator can read navRouteCoordinates = %s; want NULL", *navRoute)
		}
	})

	t.Run("drive GPS trail", func(t *testing.T) {
		var routePoints string
		if err := testPool.QueryRow(ctx,
			`SELECT "routePoints"::text FROM "Drive" WHERE "id" = $1`, f.driveID,
		).Scan(&routePoints); err != nil {
			t.Fatalf("operator SELECT on Drive: %v", err)
		}
		// routePoints is NOT NULL, so the scrubbed value is the empty array.
		if routePoints != "[]" {
			t.Errorf("operator can read the drive trail: %s", routePoints)
		}
	})

	t.Run("tesla oauth tokens", func(t *testing.T) {
		var access, refresh, id *string
		if err := testPool.QueryRow(ctx, `
			SELECT "access_token", "refresh_token", "id_token"
			FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla'`, f.userID,
		).Scan(&access, &refresh, &id); err != nil {
			t.Fatalf("operator SELECT on Account: %v", err)
		}
		for name, v := range map[string]*string{
			"access_token": access, "refresh_token": refresh, "id_token": id,
		} {
			if v != nil {
				t.Errorf("operator can read %s; a database leak would hand over "+
					"fleet control of this user's car", name)
			}
		}
	})

	// The broad sweep. The per-column assertions above only look where we
	// already know to look; this dumps every plaintext column across all
	// three tables into one string and hunts for the fixture's secrets.
	t.Run("no secret appears anywhere in the plaintext columns", func(t *testing.T) {
		dump := dumpPlaintextColumns(ctx, t)
		for label, needle := range f.needles() {
			if strings.Contains(dump, needle) {
				t.Errorf("operator can still read the %s (%q) in a plaintext column", label, needle)
			}
		}
	})

	// And the ciphertext really is ciphertext, not an encoding mistake
	// that left readable text behind.
	t.Run("ciphertext columns are opaque", func(t *testing.T) {
		var latEnc, navEnc, accessEnc string
		if err := testPool.QueryRow(ctx,
			`SELECT "latitudeEnc", "navRouteCoordinatesEnc" FROM "Vehicle" WHERE "vin" = $1`, f.vin,
		).Scan(&latEnc, &navEnc); err != nil {
			t.Fatalf("read vehicle ciphertext: %v", err)
		}
		if err := testPool.QueryRow(ctx,
			`SELECT "access_token_enc" FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla'`, f.userID,
		).Scan(&accessEnc); err != nil {
			t.Fatalf("read account ciphertext: %v", err)
		}

		for name, ct := range map[string]string{
			"latitudeEnc":            latEnc,
			"navRouteCoordinatesEnc": navEnc,
			"access_token_enc":       accessEnc,
		} {
			if ct == "" {
				t.Errorf("%s is empty — the value was not sealed at all", name)
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
			if strings.Contains(ct, f.accessToken) || strings.Contains(ct, fmt.Sprintf("%v", f.lat)) {
				t.Errorf("%s contains its own plaintext", name)
			}
		}
	})
}

// dumpPlaintextColumns concatenates every retired plaintext column across
// all three tables into one blob, so a single scan can prove a secret
// appears in none of them.
func dumpPlaintextColumns(ctx context.Context, t *testing.T) string {
	t.Helper()
	var b strings.Builder

	for _, q := range []string{
		`SELECT COALESCE("latitude"::text,'') || '|' || COALESCE("longitude"::text,'') || '|' ||
		        COALESCE("destinationLatitude"::text,'') || '|' || COALESCE("destinationLongitude"::text,'') || '|' ||
		        COALESCE("originLatitude"::text,'') || '|' || COALESCE("originLongitude"::text,'') || '|' ||
		        COALESCE("navRouteCoordinates"::text,'')
		 FROM "Vehicle"`,
		`SELECT COALESCE("routePoints"::text,'') FROM "Drive"`,
		`SELECT COALESCE("access_token",'') || '|' || COALESCE("refresh_token",'') || '|' ||
		        COALESCE("id_token",'')
		 FROM "Account"`,
	} {
		rows, err := testPool.Query(ctx, q)
		if err != nil {
			t.Fatalf("dump plaintext columns: %v", err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatalf("scan plaintext dump: %v", err)
			}
			b.WriteString(s)
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate plaintext dump: %v", err)
		}
		rows.Close()
	}
	return b.String()
}

// assertRecoverableThroughRepos proves the other half of the bar: the
// data is encrypted, not destroyed. Anyone holding the key still reads
// exactly what was written.
func assertRecoverableThroughRepos(
	ctx context.Context, t *testing.T, enc cryptox.Encryptor, logger *slog.Logger,
	f operatorFixture, when string,
) {
	t.Helper()

	vehicles := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	v, err := vehicles.GetByVIN(ctx, f.vin)
	if err != nil {
		t.Fatalf("%s: GetByVIN: %v", when, err)
	}
	if v.Latitude != f.lat || v.Longitude != f.lng {
		t.Errorf("%s: vehicle position = (%v, %v), want (%v, %v)", when, v.Latitude, v.Longitude, f.lat, f.lng)
	}
	if v.DestinationLatitude == nil || *v.DestinationLatitude != f.destLat {
		t.Errorf("%s: destination latitude not recoverable", when)
	}
	if v.OriginLatitude == nil || *v.OriginLatitude != f.originLat {
		t.Errorf("%s: origin latitude not recoverable", when)
	}
	if !jsonArraysEqual(t, v.NavRouteCoordinates, json.RawMessage(f.navRoute)) {
		t.Errorf("%s: nav route not recoverable: got %s", when, v.NavRouteCoordinates)
	}

	drives := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, logger)
	d, err := drives.GetByID(ctx, f.driveID)
	if err != nil {
		t.Fatalf("%s: GetByID: %v", when, err)
	}
	var got []store.RoutePointRecord
	if err := json.Unmarshal(d.RoutePoints, &got); err != nil {
		t.Fatalf("%s: unmarshal recovered trail (%s): %v", when, d.RoutePoints, err)
	}
	if len(got) != len(f.trail) {
		t.Fatalf("%s: recovered %d trail points, want %d", when, len(got), len(f.trail))
	}
	for i := range got {
		if got[i] != f.trail[i] {
			t.Errorf("%s: trail point %d = %+v, want %+v", when, i, got[i], f.trail[i])
		}
	}

	accounts := store.NewAccountRepo(testPool, enc)
	tok, err := accounts.GetTeslaToken(ctx, f.userID)
	if err != nil {
		t.Fatalf("%s: GetTeslaToken: %v", when, err)
	}
	if tok.AccessToken != f.accessToken {
		t.Errorf("%s: access token not recoverable", when)
	}
	if tok.RefreshToken != f.refreshToken {
		t.Errorf("%s: refresh token not recoverable", when)
	}
}

// jsonArraysEqual compares two JSON documents by decoded shape.
func jsonArraysEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("fixture nav route is not valid JSON: %v", err)
	}
	return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", bv)
}
