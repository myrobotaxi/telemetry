package store_test

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// silentGPSLogger is the logger we hand the repo so the half-pair
// warnings the tests intentionally trigger don't pollute test output.
func silentGPSLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readGPSColumns pulls the four MYR-63-relevant columns for one VIN
// directly via SQL so tests can assert on the exact (Float, Text)
// shape the dual-write path produced. Bypasses VehicleRepo so a test
// of the Read fallback can't be fooled by the same Read code that
// produced the row.
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

// TestVehicleRepo_GPS_DualWriteOnUpdate verifies that an UPDATE through
// the encryption-aware repo writes BOTH the plaintext Float column and
// the *Enc TEXT shadow for every GPS pair, and that subsequent reads
// see the same values.
func TestVehicleRepo_GPS_DualWriteOnUpdate(t *testing.T) {
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

	// Plaintext columns hold the same values they always have.
	if latPT != lat || lngPT != lng {
		t.Errorf("plaintext main = (%v,%v), want (%v,%v)", latPT, lngPT, lat, lng)
	}
	if destLatPT == nil || *destLatPT != destLat {
		t.Errorf("destinationLatitude plaintext = %v, want %v", destLatPT, destLat)
	}
	if originLatPT == nil || *originLatPT != originLat {
		t.Errorf("originLatitude plaintext = %v, want %v", originLatPT, originLat)
	}
	if destLngPT == nil || *destLngPT != destLng {
		t.Errorf("destinationLongitude plaintext = %v", destLngPT)
	}
	if originLngPT == nil || *originLngPT != originLng {
		t.Errorf("originLongitude plaintext = %v", originLngPT)
	}

	// Ciphertext columns are populated and decrypt to the floats we wrote.
	for label, ct := range map[string]*string{
		"latitudeEnc": latEnc, "longitudeEnc": lngEnc,
		"destinationLatitudeEnc": destLatEnc, "destinationLongitudeEnc": destLngEnc,
		"originLatitudeEnc": originLatEnc, "originLongitudeEnc": originLngEnc,
	} {
		if ct == nil || *ct == "" {
			t.Fatalf("%s not written", label)
		}
		plain, err := enc.DecryptString(*ct)
		if err != nil {
			t.Fatalf("decrypt %s: %v", label, err)
		}
		if _, err := strconv.ParseFloat(plain, 64); err != nil {
			t.Errorf("%s decrypt parse: %v (raw=%q)", label, err, plain)
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

// TestVehicleRepo_GPS_PlaintextFallback verifies a row with NULL *Enc
// columns reads through to the plaintext Float values — the migration-
// window legacy state.
func TestVehicleRepo_GPS_PlaintextFallback(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_003", "5YJ3E1EA1NF00GPS3")

	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, newTestEncryptor(t), silentGPSLogger())
	ctx := context.Background()

	pt := 12.34
	if _, err := testPool.Exec(ctx,
		`UPDATE "Vehicle" SET "latitude"=$1, "longitude"=$2 WHERE "vin"=$3`,
		pt, -pt, "5YJ3E1EA1NF00GPS3"); err != nil {
		t.Fatalf("seed plaintext: %v", err)
	}

	got, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00GPS3")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != pt || got.Longitude != -pt {
		t.Errorf("read = (%v,%v), want plaintext (%v,%v)", got.Latitude, got.Longitude, pt, -pt)
	}
}

// TestVehicleRepo_GPS_HalfPairEncFallsBackToPlaintext verifies the
// atomic-pair guard on the read path: when latitudeEnc is populated
// but longitudeEnc is NULL (or vice versa), the row is corrupt and
// the read falls back to plaintext for the entire pair rather than
// returning a half-pair.
func TestVehicleRepo_GPS_HalfPairEncFallsBackToPlaintext(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_gps_004", "5YJ3E1EA1NF00GPS4")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentGPSLogger())
	ctx := context.Background()

	corruptLat := 99.99
	corruptLng := -99.99
	wrongLatCT, _ := enc.EncryptString("44.44") // ciphertext that disagrees with plaintext
	if _, err := testPool.Exec(ctx,
		`UPDATE "Vehicle" SET "latitude"=$1, "longitude"=$2,
            "latitudeEnc"=$3, "longitudeEnc"=NULL WHERE "vin"=$4`,
		corruptLat, corruptLng, wrongLatCT, "5YJ3E1EA1NF00GPS4"); err != nil {
		t.Fatalf("seed half-pair: %v", err)
	}

	got, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00GPS4")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if got.Latitude != corruptLat || got.Longitude != corruptLng {
		t.Errorf("half-pair read = (%v,%v), want plaintext fallback (%v,%v)",
			got.Latitude, got.Longitude, corruptLat, corruptLng)
	}
}

// TestVehicleRepo_GPS_HalfPairInputSkipsEncWrite verifies the
// write-path mirror of the atomic-pair guard: a VehicleUpdate that
// sets only Latitude (without Longitude) writes the plaintext Float
// but leaves BOTH *Enc columns NULL (rather than encrypting one half).
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

	latPT, _, latEnc, lngEnc,
		_, _, _, _, _, _, _, _ :=
		readGPSColumns(t, testPool, "5YJ3E1EA1NF00GPS5")

	if latPT != onlyLat {
		t.Errorf("plaintext lat = %v, want %v", latPT, onlyLat)
	}
	// Atomic-pair guard: neither *Enc column should be written.
	if latEnc != nil && *latEnc != "" {
		t.Errorf("latitudeEnc unexpectedly written: %q", *latEnc)
	}
	if lngEnc != nil && *lngEnc != "" {
		t.Errorf("longitudeEnc unexpectedly written: %q", *lngEnc)
	}
}

// TestVehicleRepo_GPS_ClearFieldsAlsoClearsEnc verifies that a
// ClearFields entry for a GPS plaintext column ALSO clears its *Enc
// shadow. Otherwise navigation cancellation would leave a NULL
// plaintext + stale ciphertext, the same half-pair corruption mode
// the read path warns about.
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

// TestVehicleRepo_GPS_LegacyConstructorReadsPlaintext is the
// regression test for the "no encryptor wired" path: the legacy
// NewVehicleRepo constructor reads plaintext directly and ignores any
// *Enc column. Lets old callers keep compiling without flipping their
// behavior under us.
func TestVehicleRepo_GPS_LegacyConstructorReadsPlaintext(t *testing.T) {
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
	if got.Latitude != stalePT {
		t.Errorf("legacy repo lat = %v, want plaintext %v", got.Latitude, stalePT)
	}
}
