package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-581 — PATCH /api/users/me: the caller edits their OWN display name.
//
// THE ONE PROFILE WRITE THE PLATFORM PERMITS, by explicit client decision
// (2026-08-17), reversing MYR-366's deliberate no-rename rule. The reason it
// exists: Apple supplies a name only on the FIRST authorization, so accounts
// routinely live nameless forever and render as "Tesla" on the incoming ride
// card and the accept toast (MYR-532 item 4). The client's follow-up directive
// makes the CLIENT side mandatory — a blocking name sheet after update — so
// this endpoint is the write that gate resolves through.
//
// VALIDATION IS PERMISSIVE ABOUT THE WORLD'S NAMES. This is a person's own
// name for themselves — "José", "Ольга", "李" are all names — so there is no
// ASCII filter and no alphabet allow-list (contrast `InviteLink.inviterName`,
// which guards a URL PARAMETER and rightly drops anything odd). What is
// refused: the empty string after trimming (an empty name is a deletion this
// endpoint does not offer), control characters (nothing a keyboard types), and
// anything over maxProfileNameLength.
//
// The 200 echoes the COMMITTED name (trimmed), so the client adopts the
// server's normalization rather than its own submission.
//
// EVERY LADDER READER SEES THE NEW NAME. A successful PATCH is observable
// immediately on `RideRequest.requesterName`, `VehicleSummary.ownerFirstName`,
// `ShareInvite.acceptedByName` and `RedeemShareInviteResponse.ownerFirstName` —
// not because those readers were changed, but because the store writes every
// rung of the ladder they all share. See internal/store/user_profile_name.go.

// maxProfileNameLength bounds the stored name, in RUNES — a byte cap would
// give a CJK speaker a third of the budget a Latin one gets. Generous — it is
// a display name, not a form field — while still refusing paste accidents.
const maxProfileNameLength = 60

// maxProfileNameBodyBytes caps the request body well above any legal payload:
// a 60-rune name is at most 240 bytes of JSON-escaped UTF-8 plus the envelope.
const maxProfileNameBodyBytes = 4 << 10

// profileNameStore is the storage seam (interface at the consumer, per
// project convention). The production conformer is `store.ProfileNameRepo`.
//
// The seam is deliberately one boolean wide, and it is what let the storage
// design change underneath this handler without touching it: `UpdateUserName`
// went from one UPDATE of the Prisma `"User"` row to a transaction over every
// identity rung the account has (see internal/store/user_profile_name.go for
// why precedence, not existence, forced that). The handler's contract is
// unchanged — "did anything get named?" — only the meaning of `false` narrowed.
type profileNameStore interface {
	// UpdateUserName commits the caller's display name. false (with a nil
	// error) means the authenticated id has no identity row on ANY rung — an
	// orphaned token, not the ordinary Apple-native account.
	UpdateUserName(ctx context.Context, userID, name string) (bool, error)
}

type ProfileNameHandler struct {
	auth   tokenValidator
	store  profileNameStore
	logger *slog.Logger
}

// NewProfileNameHandler requires non-nil auth and store; only the logger may
// be nil (it falls back to slog.Default).
func NewProfileNameHandler(auth tokenValidator, store profileNameStore, logger *slog.Logger) *ProfileNameHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ProfileNameHandler{auth: auth, store: store, logger: logger}
}

type profileNameRequest struct {
	Name string `json:"name"`
}

type profileNameResponse struct {
	Name string `json:"name"`
}

// ServePatch handles PATCH /api/users/me.
func (h *ProfileNameHandler) ServePatch(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("profile name: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProfileNameBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body profileNameRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "invalid JSON body")
		return
	}
	name, refusal := normalizeProfileName(body.Name)
	if refusal != "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, refusal)
		return
	}

	updated, err := h.store.UpdateUserName(r.Context(), userID, name)
	if err != nil {
		h.logger.Error("profile name update failed", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "could not update name")
		return
	}
	if !updated {
		// THE 404 ARM'S MEANING CHANGED WITH THE STORE (MYR-581 follow-up).
		//
		// It used to mean "no Prisma `User` row", which was the ORDINARY state of
		// every Apple-native account — so the endpoint 404'd at exactly the
		// nameless population it exists to serve. The store now writes every
		// identity rung the account has a row on, so zero rows means the
		// authenticated id has NO row in `"User"`, `go_identity_apple` or
		// `go_users`: an orphaned token (a deleted account whose access token has
		// not yet expired, or a bootstrap-override id that was never provisioned).
		//
		// Still a 404 and still not a client error — there is no account to name —
		// but it is now genuinely an account-lifecycle anomaly rather than a
		// routine one, and a client meeting it should re-authenticate rather than
		// retry the write.
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "account not found")
		return
	}

	// The name is user content — log the EVENT, never the value.
	h.logger.Info("profile name updated", slog.String("user_id", userID))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(profileNameResponse{Name: name})
}

// normalizeProfileName trims and validates. Returns the committed name, or a
// refusal message.
func normalizeProfileName(raw string) (name, refusal string) {
	name = strings.TrimSpace(raw)
	if name == "" {
		return "", "name must not be empty"
	}
	if utf8.RuneCountInString(name) > maxProfileNameLength {
		return "", "name is too long"
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", "name contains control characters"
		}
	}
	return name, ""
}

func (h *ProfileNameHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
