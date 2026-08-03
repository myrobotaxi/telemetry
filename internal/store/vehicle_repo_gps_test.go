package store_test

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// silentGPSLogger is the logger we hand the repo so the half-pair
// warnings the tests intentionally trigger don't pollute test output.
func silentGPSLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readGPSColumns pulls the MYR-63-relevant columns for one VIN directly
// via SQL. After MYR-433 the server writes only the *Enc side, so raw SQL
// is the only way to prove the retired plaintext columns were genuinely
// left alone — VehicleRepo no longer selects them, and a test that read
// through the repo could not tell "never written" from "not projected".
func readGPSColumns(t *testing.T, pool *pgxpool.Pool, vin string) (latPT, lngPT float64, latEnc, lngEnc *string,
	destLatPT, destLngPT *float64, destLatEnc, destLngEnc *string,
	originLatPT, originLngPT *float64, originLatEnc, originLngEnc *string,
) {
	t.Helper()
	row := pool.QueryRow(context.Background(),
		`SELECT "latitude", "longitude", "latitudeEnc", "longitudeEnc",
                "destinationLatitude", "destinationLongitude", "destinationLatitudeEnc", "destinationLongitudeEnc",
                "originLatitude", "originLongitude", "originLatitudeEnc", "originLongitudeEnc"
         FROM "Vehicle" WHERE "vin" = $1`, vin)
	if err := row.Scan(&latPT, &lngPT, &latEnc, &lngEnc,
		&destLatPT, &destLngPT, &destLatEnc, &destLngEnc,
		&originLatPT, &originLngPT, &originLatEnc, &originLngEnc); err != nil {
		t.Fatalf("readGPSColumns(%s): %v", vin, err)
	}
	return
}

// TestVehicleRepo_GPS_WritesCiphertextOnly pins the MYR-433 write
// contract: an UPDATE through the encryption-aware repo populates the
// *Enc TEXT columns and does not touch the retired plaintext Float
// columns at all.
//
// The plaintext columns staying at their seeded values is the assertion
// that matters. The point of MYR-433 is that an operator holding a
// database dump cannot read a vehicle's coordinates; that only holds if
// the server stops depositing a readable copy on every telemetry tick.
// "Ciphertext is written" alone would still pass if the plaintext were
// being written alongside it, which is exactly the state we left behind.
func TestVehicleRepo_GPS_WritesCiphertextOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_001", "5YJ3E1EA1NF00GPS1")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentGPSLogger())
	ctx := context.Background()

	lat, lng := 33.0975, -96.8214
	destLat, destLng := 32.78, -96.80
	originLat, originLng := 33.10, -96.83
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00GPS1", store.VehicleUpdate{
		Latitude:             &lat,
		Longitude:            &lng,
		DestinationLatitude:  &destLat,
		DestinationLongitude: &destLng,
		OriginLatitude:       &originLat,
		OriginLongitude:      &originLng,
		LastUpdated:          time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry: %v", err)
	}

	latPT, lngPT, latEnc, lngEnc,
		destLatPT, destLngPT, destLatEnc, destLngEnc,
		originLatPT, originLngPT, originLatEnc, originLngEnc :=
		readGPSColumns(t, testPool, "5YJ3E1EA1NF00GPS1")

	// Plaintext columns are untouched by the server: latitude/longitude are
	// NOT NULL on the Prisma schema and stay at the seeded 0; the nullable
	// destination/origin columns stay NULL.
	if latPT != 0 || lngPT != 0 {
		t.Errorf("plaintext main = (%v,%v), want (0,0) — server must not write plaintext GPS", latPT, lngPT)
	}
	for label, pt := range map[string]*float64{
		"destinationLatitude": destLatPT, "destinationLongitude": destLngPT,
		"originLatitude": originLatPT, "originLongitude": originLngPT,
	} {
		if pt != nil {
			t.Errorf("%s plaintext = %v, want NULL — server must not write plaintext GPS", label, *pt)
		}
	}

	// Ciphertext columns are populated and decrypt to the floats we wrote.
	for label, tc := range map[string]struct {
		ct   *string
		want float64
	}{
		"latitudeEnc":             {latEnc, lat},
		"longitudeEnc":            {lngEnc, lng},
		"destinationLatitudeEnc":  {destLatEnc, destLat},
		"destinationLongitudeEnc": {destLngEnc, destLng},
		"originLatitudeEnc":       {originLatEnc, originLat},
		"originLongitudeEnc":      {originLngEnc, originLng},
	} {
		if tc.ct == nil || *tc.ct == "" {
			t.Fatalf("%s not written", label)
		}
		plain, err := enc.DecryptString(*tc.ct)
		if err != nil {
			t.Fatalf("decrypt %s: %v", label, err)
		}
		got, err := strconv.ParseFloat(plain, 64)
		if err != nil {
			t.Errorf("%s decrypt parse: %v (raw=%q)", label, err, plain)
			continue
		}
		if got != tc.want {
			t.Errorf("%s decrypts to %v, want %v", label, got, tc.want)
		}
	}

	// Read path returns the resolved values.
	got, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00GPS1")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != lat || got.Longitude != lng {
		t.Errorf("read main = (%v,%v), want (%v,%v)", got.Latitude, got.Longitude, lat, lng)
	}
	if got.DestinationLatitude == nil || *got.DestinationLatitude != destLat {
		t.Errorf("read destLat = %v, want %v", got.DestinationLatitude, destLat)
	}
	if got.OriginLongitude == nil || *got.OriginLongitude != originLng {
		t.Errorf("read originLng = %v, want %v", got.OriginLongitude, originLng)
	}
}

// TestVehicleRepo_GPS_PrefersCiphertextOnRead verifies the read path
// prefers the *Enc shadow when both halves are populated. Plaintext
// holds STALE values to make the preference observable.
func TestVehicleRepo_GPS_PrefersCiphertextOnRead(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_002", "5YJ3E1EA1NF00GPS2")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentGPSLogger())
	ctx := context.Background()

	// Stale plaintext values vs. the encrypted "real" values. The
	// read should return the encrypted ones.
	staleLat, staleLng := 1.0, 2.0
	realLat, realLng := 33.5, -96.5

	realLatCT, _ := enc.EncryptString(strconv.FormatFloat(realLat, 'g', -1, 64))
	realLngCT, _ := enc.EncryptString(strconv.FormatFloat(realLng, 'g', -1, 64))

	if _, err := testPool.Exec(ctx,
		`UPDATE "Vehicle" SET "latitude"=$1, "longitude"=$2, "latitudeEnc"=$3, "longitudeEnc"=$4 WHERE "vin"=$5`,
		staleLat, staleLng, realLatCT, realLngCT, "5YJ3E1EA1NF00GPS2"); err != nil {
		t.Fatalf("seed stale plaintext + ciphertext: %v", err)
	}

	got, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00GPS2")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != realLat || got.Longitude != realLng {
		t.Errorf("read = (%v,%v), want ciphertext-resolved (%v,%v)",
			got.Latitude, got.Longitude, realLat, realLng)
	}
}

// TestVehicleRepo_GPS_NoPlaintextFallback is the MYR-433 acceptance
// behaviour, and the inverse of the test that used to live here.
//
// A row whose *Enc columns are NULL but whose retired plaintext columns
// still hold coordinates (a pre-rollout row that was never backfilled)
// must surface NO location. The pre-MYR-433 read fell back to those
// columns, which meant the plaintext had to stay readable for the product
// to work — the fallback WAS the leak. Losing a stale coordinate on an
// un-backfilled row is the accepted cost; the backfill job
// (cmd/backfill-vehicle-gps) is what closes that gap, not the read path.
func TestVehicleRepo_GPS_NoPlaintextFallback(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_003", "5YJ3E1EA1NF00GPS3")

	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, newTestEncryptor(t), silentGPSLogger())
	ctx := context.Background()

	pt := 12.34
	if _, err := testPool.Exec(ctx,
		`UPDATE "Vehicle" SET "latitude"=$1, "longitude"=$2,
            "destinationLatitude"=$1, "destinationLongitude"=$2,
            "originLatitude"=$1, "originLongitude"=$2 WHERE "vin"=$3`,
		pt, -pt, "5YJ3E1EA1NF00GPS3"); err != nil {
		t.Fatalf("seed plaintext: %v", err)
	}

	got, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00GPS3")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != 0 || got.Longitude != 0 {
		t.Errorf("read = (%v,%v), want (0,0) — plaintext must never surface", got.Latitude, got.Longitude)
	}
	for label, p := range map[string]*float64{
		"DestinationLatitude": got.DestinationLatitude, "DestinationLongitude": got.DestinationLongitude,
		"OriginLatitude": got.OriginLatitude, "OriginLongitude": got.OriginLongitude,
	} {
		if p != nil {
			t.Errorf("%s = %v, want nil — plaintext must never surface", label, *p)
		}
	}
}

// TestVehicleRepo_GPS_HalfPairEncYieldsNoLocation verifies the
// atomic-pair guard on the read path: when latitudeEnc is populated but
// longitudeEnc is NULL (or vice versa), the row is corrupt and the read
// surfaces NO location for the whole pair.
//
// Two invariants are riding on this. The atomic-pair rule (a consumer
// must never plot a latitude against a mismatched longitude) is the
// original one. MYR-433 adds the second: "corrupt" must not mean "reach
// for the plaintext column" — the plaintext columns seeded below are
// deliberately non-zero so a regression that resurrects the fallback
// shows up as a coordinate instead of a zero.
func TestVehicleRepo_GPS_HalfPairEncYieldsNoLocation(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_004", "5YJ3E1EA1NF00GPS4")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentGPSLogger())
	ctx := context.Background()

	decoyLat := 99.99
	decoyLng := -99.99
	wrongLatCT, _ := enc.EncryptString("44.44") // ciphertext that disagrees with plaintext
	if _, err := testPool.Exec(ctx,
		`UPDATE "Vehicle" SET "latitude"=$1, "longitude"=$2,
            "latitudeEnc"=$3, "longitudeEnc"=NULL WHERE "vin"=$4`,
		decoyLat, decoyLng, wrongLatCT, "5YJ3E1EA1NF00GPS4"); err != nil {
		t.Fatalf("seed half-pair: %v", err)
	}

	got, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00GPS4")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != 0 || got.Longitude != 0 {
		t.Errorf("half-pair read = (%v,%v), want (0,0): neither the surviving ciphertext half "+
			"nor the plaintext decoy (%v,%v) may be surfaced",
			got.Latitude, got.Longitude, decoyLat, decoyLng)
	}
}

// TestVehicleRepo_GPS_HalfPairInputSkipsEncWrite verifies the write-path
// mirror of the atomic-pair guard: a VehicleUpdate that sets only
// Latitude (without Longitude) leaves BOTH *Enc columns NULL rather than
// encrypting one half of a pair.
//
// Post-MYR-433 the skipped half-pair is written NOWHERE — the plaintext
// column is not a consolation prize. A latitude landing in the plaintext
// column here would be both a leak and a lie, since the read path would
// never surface it.
func TestVehicleRepo_GPS_HalfPairInputSkipsEncWrite(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_005", "5YJ3E1EA1NF00GPS5")

	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, newTestEncryptor(t), silentGPSLogger())
	ctx := context.Background()

	onlyLat := 50.0
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00GPS5", store.VehicleUpdate{
		Latitude:    &onlyLat,
		LastUpdated: time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry: %v", err)
	}

	latPT, lngPT, latEnc, lngEnc,
		_, _, _, _, _, _, _, _ :=
		readGPSColumns(t, testPool, "5YJ3E1EA1NF00GPS5")

	// Atomic-pair guard: neither *Enc column should be written.
	if latEnc != nil && *latEnc != "" {
		t.Errorf("latitudeEnc unexpectedly written: %q", *latEnc)
	}
	if lngEnc != nil && *lngEnc != "" {
		t.Errorf("longitudeEnc unexpectedly written: %q", *lngEnc)
	}
	// …and the retired plaintext columns are no consolation prize: the
	// half-pair latitude must not land there either.
	if latPT != 0 || lngPT != 0 {
		t.Errorf("plaintext = (%v,%v), want (0,0) — the dropped half-pair (%v) must not be written anywhere",
			latPT, lngPT, onlyLat)
	}
}

// TestVehicleRepo_GPS_ClearFieldsAlsoClearsEnc verifies that a
// ClearFields entry named after a GPS plaintext column clears the *Enc
// column that replaced it. ClearFields still speaks the plaintext column
// vocabulary (that is what the nav-field tables are keyed on), but after
// MYR-433 the ciphertext is the only copy — so a cancel that missed it
// would leave the read path serving a route the driver already ended.
//
// The plaintext assertions below are a belt-and-braces check that the
// columns stayed NULL throughout: the seeding UpdateTelemetry never wrote
// them, and the clear must not start writing them either.
func TestVehicleRepo_GPS_ClearFieldsAlsoClearsEnc(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_006", "5YJ3E1EA1NF00GPS6")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentGPSLogger())
	ctx := context.Background()

	// Seed an encrypted destination pair.
	destLat, destLng := 32.78, -96.80
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00GPS6", store.VehicleUpdate{
		DestinationLatitude:  &destLat,
		DestinationLongitude: &destLng,
		LastUpdated:          time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry seed: %v", err)
	}

	// Now clear the destination — navigation cancelled.
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00GPS6", store.VehicleUpdate{
		ClearFields: []string{"destinationLatitude", "destinationLongitude", "destinationAddress"},
		LastUpdated: time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry clear: %v", err)
	}

	_, _, _, _, destLatPT, destLngPT, destLatEnc, destLngEnc, _, _, _, _ :=
		readGPSColumns(t, testPool, "5YJ3E1EA1NF00GPS6")
	if destLatPT != nil {
		t.Errorf("destinationLatitude not cleared: %v", *destLatPT)
	}
	if destLngPT != nil {
		t.Errorf("destinationLongitude not cleared: %v", *destLngPT)
	}
	if destLatEnc != nil {
		t.Errorf("destinationLatitudeEnc not cleared: %q", *destLatEnc)
	}
	if destLngEnc != nil {
		t.Errorf("destinationLongitudeEnc not cleared: %q", *destLngEnc)
	}
}

// TestVehicleRepo_GPS_LegacyConstructorReadsNothing pins what the "no
// encryptor wired" path does after MYR-433: nothing. The legacy
// NewVehicleRepo constructor still compiles for callers that have no
// Encryptor in scope, but it can no longer surface coordinates — it
// cannot decrypt the *Enc columns, and the plaintext columns it used to
// read are not in the projection any more.
//
// A code path that quietly kept working without a key would be the whole
// leak re-opened under a different constructor, so "reads nothing" is the
// intended, load-bearing outcome rather than a degradation to tolerate.
// Callers that need GPS must be wired with NewVehicleRepoWithEncryption.
func TestVehicleRepo_GPS_LegacyConstructorReadsNothing(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_007", "5YJ3E1EA1NF00GPS7")

	enc := newTestEncryptor(t)
	stalePT := 7.7
	realCT, _ := enc.EncryptString("8.8")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE "Vehicle" SET "latitude"=$1, "longitude"=$1, "latitudeEnc"=$2, "longitudeEnc"=$2 WHERE "vin"=$3`,
		stalePT, realCT, "5YJ3E1EA1NF00GPS7"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	legacyRepo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	got, err := legacyRepo.GetByVIN(context.Background(), "5YJ3E1EA1NF00GPS7")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != 0 || got.Longitude != 0 {
		t.Errorf("legacy repo read = (%v,%v), want (0,0): neither the ciphertext it cannot decrypt "+
			"nor the plaintext decoy (%v) may be surfaced", got.Latitude, got.Longitude, stalePT)
	}
}
