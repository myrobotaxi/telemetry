// The declarative purge target table, split out of purge.go (which had
// outgrown the 300-line cap once MYR-447 added the eight location-label
// columns). purge.go owns the engine; this file owns the data it walks.

package plaintextpurge

// Targets is the full set of plaintext columns MYR-433 and MYR-447
// retire, in a deliberate order: Tesla OAuth tokens first.
//
// The token columns are the sharpest item in the database. Vehicle GPS
// and route blobs reveal where someone went; a Tesla access/refresh token
// grants control of their car. If a purge run is interrupted, the
// credentials should already be gone.
var Targets = []Target{
	{Table: "Account", Name: "access_token", Members: []Member{
		{Plaintext: "access_token", Encrypted: "access_token_enc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"access_token" IS NOT NULL`},
	}},
	{Table: "Account", Name: "refresh_token", Members: []Member{
		{Plaintext: "refresh_token", Encrypted: "refresh_token_enc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"refresh_token" IS NOT NULL`},
	}},
	{Table: "Account", Name: "id_token", Members: []Member{
		{Plaintext: "id_token", Encrypted: "id_token_enc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"id_token" IS NOT NULL`},
	}},

	// The three GPS pairs. Each is one target so a half-verifying pair
	// cannot leave one readable coordinate behind.
	{Table: "Vehicle", Name: "latitude+longitude", Members: []Member{
		{Plaintext: "latitude", Encrypted: "latitudeEnc", Kind: KindFloat,
			ScrubSQL: "0", HasData: `"latitude" IS NOT NULL AND "latitude" <> 0`},
		{Plaintext: "longitude", Encrypted: "longitudeEnc", Kind: KindFloat,
			ScrubSQL: "0", HasData: `"longitude" IS NOT NULL AND "longitude" <> 0`},
	}},
	{Table: "Vehicle", Name: "destinationLatitude+destinationLongitude", Members: []Member{
		{Plaintext: "destinationLatitude", Encrypted: "destinationLatitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"destinationLatitude" IS NOT NULL`},
		{Plaintext: "destinationLongitude", Encrypted: "destinationLongitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"destinationLongitude" IS NOT NULL`},
	}},
	{Table: "Vehicle", Name: "originLatitude+originLongitude", Members: []Member{
		{Plaintext: "originLatitude", Encrypted: "originLatitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"originLatitude" IS NOT NULL`},
		{Plaintext: "originLongitude", Encrypted: "originLongitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"originLongitude" IS NOT NULL`},
	}},

	{Table: "Vehicle", Name: "navRouteCoordinates", Members: []Member{
		{Plaintext: "navRouteCoordinates", Encrypted: "navRouteCoordinatesEnc", Kind: KindJSON,
			ScrubSQL: "NULL",
			HasData:  `"navRouteCoordinates" IS NOT NULL AND "navRouteCoordinates"::text NOT IN ('[]', 'null')`},
	}},
	{Table: "Drive", Name: "routePoints", Members: []Member{
		{Plaintext: "routePoints", Encrypted: "routePointsEnc", Kind: KindJSON,
			ScrubSQL: `'[]'::jsonb`,
			HasData:  `"routePoints" IS NOT NULL AND "routePoints"::text NOT IN ('[]', 'null')`},
	}},

	// MYR-447 — the geocoded location labels. Each is its own target,
	// deliberately NOT grouped the way the GPS pairs are.
	//
	// A GPS pair is grouped because a latitude without its longitude is a
	// broken row: scrubbing the verified half would both leave a readable
	// coordinate and half-destroy the pair, so refusing is strictly
	// better. Labels have neither property. Each one is an independently
	// recoverable string, and each one on its own already gives away the
	// location — "Alcatraz Landing" and "Pier 33, San Francisco" are two
	// spellings of the same disclosure, not two halves of one. Grouping
	// them would mean one undecryptable column pins the other three in
	// plaintext, which is the wrong trade in both directions: scrubbing
	// what verifies strictly reduces exposure and destroys nothing.
	//
	// ScrubSQL tracks each column's nullability in the react-frontend
	// Prisma schema, because an all-or-nothing UPDATE that violates a NOT
	// NULL constraint aborts the whole target:
	//
	//	Vehicle.locationName        String  @default("")  → NOT NULL → ''
	//	Vehicle.locationAddress     String  @default("")  → NOT NULL → ''
	//	Vehicle.destinationName     String?               → nullable → NULL
	//	Vehicle.destinationAddress  String?               → nullable → NULL
	//	Drive.startLocation         String                → NOT NULL → ''
	//	Drive.startAddress          String                → NOT NULL → ''
	//	Drive.endLocation           String                → NOT NULL → ''
	//	Drive.endAddress            String                → NOT NULL → ''
	//
	// The empty string is the correct "no data here" value for the NOT
	// NULL columns and reveals no location, exactly as the zero
	// coordinate does for latitude/longitude.
	//
	// HasData excludes the empty string as well as NULL: an empty label
	// discloses nothing, so churning those rows would inflate the
	// operator's counters with work that changes nothing.
	{Table: "Vehicle", Name: "locationName", Members: []Member{
		{Plaintext: "locationName", Encrypted: "locationNameEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"locationName" IS NOT NULL AND "locationName" <> ''`},
	}},
	{Table: "Vehicle", Name: "locationAddress", Members: []Member{
		{Plaintext: "locationAddress", Encrypted: "locationAddressEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"locationAddress" IS NOT NULL AND "locationAddress" <> ''`},
	}},
	{Table: "Vehicle", Name: "destinationName", Members: []Member{
		{Plaintext: "destinationName", Encrypted: "destinationNameEnc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"destinationName" IS NOT NULL AND "destinationName" <> ''`},
	}},
	{Table: "Vehicle", Name: "destinationAddress", Members: []Member{
		{Plaintext: "destinationAddress", Encrypted: "destinationAddressEnc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"destinationAddress" IS NOT NULL AND "destinationAddress" <> ''`},
	}},
	{Table: "Drive", Name: "startLocation", Members: []Member{
		{Plaintext: "startLocation", Encrypted: "startLocationEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"startLocation" IS NOT NULL AND "startLocation" <> ''`},
	}},
	{Table: "Drive", Name: "startAddress", Members: []Member{
		{Plaintext: "startAddress", Encrypted: "startAddressEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"startAddress" IS NOT NULL AND "startAddress" <> ''`},
	}},
	{Table: "Drive", Name: "endLocation", Members: []Member{
		{Plaintext: "endLocation", Encrypted: "endLocationEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"endLocation" IS NOT NULL AND "endLocation" <> ''`},
	}},
	{Table: "Drive", Name: "endAddress", Members: []Member{
		{Plaintext: "endAddress", Encrypted: "endAddressEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"endAddress" IS NOT NULL AND "endAddress" <> ''`},
	}},
}
