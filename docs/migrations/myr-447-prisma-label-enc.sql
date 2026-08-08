-- MYR-447 — encrypted shadows for the geocoded human-readable location
-- labels on the Prisma-owned "Vehicle" and "Drive" tables.
--
-- ============================================================
-- THIS FILE IS A DEPLOY ARTIFACT, NOT A GO MIGRATION.
-- ============================================================
--
-- "Vehicle" and "Drive" are owned by the Next.js app's Prisma schema.
-- docs/architecture/migrations.md §4.2 and contract-guard rule CG-DL-9
-- forbid any file under internal/store/migrations/ from so much as naming
-- those tables — CREATE, ALTER, DROP, INSERT, UPDATE or DELETE. So this
-- server cannot add these columns itself.
--
-- The DDL below MUST be applied as a Prisma migration in the sibling
-- `react-frontend` repo (a new directory under prisma/migrations/,
-- alongside 20260509175411_add_vehicle_gps_enc_columns and
-- 20260509182821_add_route_blob_enc_columns) and that migration MUST be
-- deployed BEFORE the telemetry server carrying MYR-447 ships.
--
-- Deploy order is load-bearing, not advisory. From the MYR-447 commit
-- onwards the server SELECTs and UPDATEs these `*Enc` columns on its hot
-- read paths — GET /api/vehicles/{id}/snapshot, the WS vehicle_update
-- snapshot replay, GET /api/vehicles/{id}/drives and GET /api/drives/{id}.
-- Against a database that has not run this DDL every one of those reads
-- fails outright with `column "locationNameEnc" does not exist`. There is
-- no degraded mode.
--
-- Recommended sequence:
--   1. Apply this DDL in react-frontend (Prisma migrate deploy).
--   2. Run `backfill-location-labels` against the same database so the
--      existing plaintext labels are sealed into the new columns. Until
--      it runs, historic rows read as "no label" (an empty string), and
--      the geocode backfill (`ops geocode backfill`) will regard every
--      unsealed Drive row as missing an address.
--   3. Deploy the telemetry server.
--   4. Run `purge-plaintext-columns` to scrub the retired plaintext.
--   5. Drop the plaintext columns in a later react-frontend migration,
--      once step 4 reports zero remaining.
--
-- Column contract (fixed by MYR-433): `<plaintextColumnName>Enc`, TEXT,
-- NULLABLE. NULL is the absent sentinel — it is what makes
-- queryDriveMissingAddresses' discovery predicate expressible, because
-- AES-GCM ciphertext is nondeterministic (fresh nonce per encrypt) and
-- can therefore never be matched with `= ''`.
--
-- Ciphertext format owned by lib/cryptox (TS) and internal/cryptox (Go):
--   [1B version=0x01][12B nonce][N B ct + 16B tag], base64(StdEncoding).
-- The empty string is the absent sentinel at the encryption boundary:
-- EncryptString("") returns "", which this server persists as NULL.
--
-- Additive and idempotent — no rewrite of existing rows, safe to re-apply.

ALTER TABLE "Vehicle"
  ADD COLUMN IF NOT EXISTS "locationNameEnc"       TEXT,
  ADD COLUMN IF NOT EXISTS "locationAddressEnc"    TEXT,
  ADD COLUMN IF NOT EXISTS "destinationNameEnc"    TEXT,
  ADD COLUMN IF NOT EXISTS "destinationAddressEnc" TEXT;

ALTER TABLE "Drive"
  ADD COLUMN IF NOT EXISTS "startLocationEnc" TEXT,
  ADD COLUMN IF NOT EXISTS "startAddressEnc"  TEXT,
  ADD COLUMN IF NOT EXISTS "endLocationEnc"   TEXT,
  ADD COLUMN IF NOT EXISTS "endAddressEnc"    TEXT;
