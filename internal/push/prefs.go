package push

import "context"

// Notification preferences (MYR-349).
//
// Every notification this service can send belongs to exactly ONE category, and
// the recipient can switch each category off. The categories are the five
// switches the app's two Settings screens render; the mapping from a send site
// to its category is fixed here rather than at the send site, so a new notifier
// cannot ship without answering the question "which switch turns this off?".

// Category names one notification concern. The string values are the
// go_push_prefs column names (migration 0022) and the §7.19 wire keys' snake
// form — they are persisted and contract-visible, so they are not renameable.
type Category string

const (
	// CategoryRideLifecycle covers the WHOLE ride status class: a new request
	// reaching an owner, and accepted / declined / arrived / enroute /
	// completed / expired reaching a rider. All three of this package's
	// fan-out sites are in it.
	//
	// It is ONE category on purpose. The rider's two switches ("Request
	// accepted / declined", "Pick-up & arrival alerts") both resolve here,
	// because no send site distinguishes them: `arrived` and `accepted` travel
	// the same handler, the same alert builder and the same fan-out. Minting a
	// second column would create a preference the server could store and never
	// honour differently, which is the exact class of lie MYR-349 exists to
	// remove.
	CategoryRideLifecycle Category = "ride_lifecycle"

	// CategoryDriveStarted — the owner's car began a drive.
	CategoryDriveStarted Category = "drive_started"
	// CategoryDriveCompleted — the owner's car finished a drive.
	CategoryDriveCompleted Category = "drive_completed"
	// CategoryChargingComplete — the owner's car finished charging.
	CategoryChargingComplete Category = "charging_complete"
	// CategoryViewerJoined — somebody redeemed an invite to the owner's car.
	CategoryViewerJoined Category = "viewer_joined"
)

// Categories is every category, in the order §7.19 renders them. Used by the
// handler to build a response and by tests to prove the set is exhaustive.
func Categories() []Category {
	return []Category{
		CategoryRideLifecycle,
		CategoryDriveStarted,
		CategoryDriveCompleted,
		CategoryChargingComplete,
		CategoryViewerJoined,
	}
}

// Prefs is one recipient's answers.
//
// The zero value is NOT a usable Prefs — it means "everything off", which is
// the opposite of what an account with no stored row wants. Build it with
// DefaultPrefs or from a store read.
type Prefs struct {
	RideLifecycle    bool
	DriveStarted     bool
	DriveCompleted   bool
	ChargingComplete bool
	ViewerJoined     bool
}

// DefaultPrefs is the pre-MYR-349 behaviour: every category on. It is what an
// account with no stored row gets, and what the notifier falls back to whenever
// a preference cannot be resolved.
func DefaultPrefs() Prefs {
	return Prefs{
		RideLifecycle:    true,
		DriveStarted:     true,
		DriveCompleted:   true,
		ChargingComplete: true,
		ViewerJoined:     true,
	}
}

// Allows reports whether a notification in this category may be delivered.
//
// An UNKNOWN category returns true. That is deliberate and it is the safe
// direction: the only way to reach it is a send site added with a category the
// preference model has not grown a column for yet, and in that situation the
// notification going out is a bug in the wrong direction by far less than the
// notification being silently swallowed for every user on the platform.
func (p Prefs) Allows(c Category) bool {
	switch c {
	case CategoryRideLifecycle:
		return p.RideLifecycle
	case CategoryDriveStarted:
		return p.DriveStarted
	case CategoryDriveCompleted:
		return p.DriveCompleted
	case CategoryChargingComplete:
		return p.ChargingComplete
	case CategoryViewerJoined:
		return p.ViewerJoined
	default:
		return true
	}
}

// PrefStore resolves a recipient's preferences. Declared here (consumer site)
// so internal/push never imports internal/store; the cmd/ wiring adapts the
// store row onto this shape, exactly as it does for DeviceStore.
//
// An account with no stored row MUST yield DefaultPrefs and a nil error — the
// missing row is the ordinary state, not a failure.
type PrefStore interface {
	PrefsForUser(ctx context.Context, userID string) (Prefs, error)
}
