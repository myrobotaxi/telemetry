// Drive location-label seal/open helpers (MYR-447). Split out of
// drive_repo.go, which was already at the file-size cap.
//
// A Drive row carries four geocoded labels — the place name and street
// address of where the drive began and where it ended. MYR-433 sealed the
// breadcrumb trail between those two points but left the endpoints
// themselves spelled out in English one column over, which made the
// trail's encryption largely ceremonial: "started at 1600 Pennsylvania
// Ave, ended at Alcatraz Landing" is the part of a drive a human actually
// wants to read.
//
// All four are now ciphertext-only. The retired plaintext columns are
// NOT NULL on the Prisma schema, so the INSERT still writes empty strings
// into them (see queryDriveInsert) and `cmd/purge-plaintext-columns`
// scrubs the historic residue.

package store

import "fmt"

// sealDriveLabels encrypts the four Drive labels in the fixed order
// queryDriveInsert expects: startLocation, startAddress, endLocation,
// endAddress. Each element is nil when the label was empty, which the
// INSERT writes as NULL — the absent sentinel that
// queryDriveMissingAddresses' `IS NULL` discovery predicate depends on.
//
// A repo built without an Encryptor returns four nils rather than an
// error. Create() must still work for a keyless repo (drive detection
// does not stop because a key is missing), and a drive row with no
// endpoint labels is exactly what a keyless repo has always produced for
// coordinates. The labels are recoverable later by re-running the
// geocode backfill.
func (r *DriveRepo) sealDriveLabels(startLoc, startAddr, endLoc, endAddr string) ([4]*string, error) {
	var out [4]*string
	if r.encryptor == nil {
		return out, nil
	}
	for i, plain := range [4]string{startLoc, startAddr, endLoc, endAddr} {
		ct, err := labelToEncPtr(plain, r.encryptor)
		if err != nil {
			return [4]*string{}, fmt.Errorf("seal drive label %s: %w", driveLabelColumns[i], err)
		}
		out[i] = ct
	}
	return out, nil
}

// sealEndLabels is the Complete()-path pair: the two end-side labels,
// sealed. Same keyless behaviour as sealDriveLabels — nil parameters
// leave the columns NULL rather than failing the drive completion.
func (r *DriveRepo) sealEndLabels(endLoc, endAddr string) (loc, addr *string, err error) {
	if r.encryptor == nil {
		return nil, nil, nil
	}
	loc, err = labelToEncPtr(endLoc, r.encryptor)
	if err != nil {
		return nil, nil, fmt.Errorf("seal drive label endLocation: %w", err)
	}
	addr, err = labelToEncPtr(endAddr, r.encryptor)
	if err != nil {
		return nil, nil, fmt.Errorf("seal drive label endAddress: %w", err)
	}
	return loc, addr, nil
}

// sealOptionalLabels is the UpdateAddresses-path variant: each input is
// already nilable ("the caller did not geocode this side this run"), and
// a nil stays nil so the query's COALESCE leaves the column untouched.
//
// A non-nil but EMPTY label also maps to nil. It carries no address, and
// writing an empty ciphertext would flip the column non-NULL and hide the
// row from the backfill's `IS NULL` discovery predicate forever while
// recording nothing.
//
// Order matches queryDriveUpdateAddresses: startLocation, startAddress,
// endLocation, endAddress.
func (r *DriveRepo) sealOptionalLabels(vals ...*string) ([4]*string, error) {
	var out [4]*string
	for i, v := range vals {
		if v == nil || *v == "" {
			continue
		}
		ct, err := labelToEncPtr(*v, r.encryptor)
		if err != nil {
			return [4]*string{}, fmt.Errorf("seal drive label %s: %w", driveLabelColumns[i], err)
		}
		out[i] = ct
	}
	return out, nil
}

// applyResolvedDriveLabels decrypts the four `*Enc` label columns onto a
// DriveRecord.
//
// All four DriveRecord fields are plain strings (the Prisma columns are
// NOT NULL), so an absent, empty or unreadable ciphertext collapses to ""
// — indistinguishable from the pre-MYR-447 value of a drive whose
// endpoints were never geocoded, which is what keeps the wire shape
// byte-identical. A repo built without an Encryptor reads no labels.
func (r *DriveRepo) applyResolvedDriveLabels(d *DriveRecord, startLoc, startAddr, endLoc, endAddr *string) {
	if r.encryptor == nil {
		return
	}
	d.StartLocation = r.openDriveLabel(startLoc, "startLocationEnc")
	d.StartAddress = r.openDriveLabel(startAddr, "startAddressEnc")
	d.EndLocation = r.openDriveLabel(endLoc, "endLocationEnc")
	d.EndAddress = r.openDriveLabel(endAddr, "endAddressEnc")
}

// openDriveLabel is the DriveRepo-bound spelling of encStringToLabel, so
// the four call sites above (and scanDriveSummaryRow's) don't each repeat
// the encryptor/logger/metrics triple.
func (r *DriveRepo) openDriveLabel(ct *string, encColumn string) string {
	return encStringToLabel(ct, r.encryptor, r.logger, r.metrics, encColumn)
}
