package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupSavedPlacesEndpoints mounts the account-persisted saved-place surface
// (MYR-321, rest-api.md §7.20):
//
//	GET    /api/users/me/places
//	PUT    /api/users/me/places/{kind}
//	DELETE /api/users/me/places/{kind}
//
// ALWAYS MOUNTED, like the other /users/me surfaces: every operation is a local
// database read or write, with no proxy, no Tesla call and no optional
// dependency that could make the endpoint unsafe to expose.
//
// The encryptor is NOT optional here, and that is enforced by construction:
// go_saved_places stores coordinates encrypt-only, so NewSavedPlacesRepo panics
// on a nil Encryptor rather than silently writing somebody's home address in
// the clear. deps.encryptor is non-nil for every real deployment (it is the
// same KeySet the ride and vehicle paths already require).
func setupSavedPlacesEndpoints(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "saved-places"))

	repo := store.NewSavedPlacesRepo(deps.pool, deps.encryptor)
	handler := telemetry.NewSavedPlacesHandler(
		deps.authenticator,
		&savedPlacesAdapter{repo: repo},
		logger,
	)

	deps.srv.HandleFunc("GET /api/users/me/places", handler.ServeList)
	deps.srv.HandleFunc("PUT /api/users/me/places/{kind}", handler.ServePut)
	deps.srv.HandleFunc("DELETE /api/users/me/places/{kind}", handler.ServeDelete)
	logger.Info("saved place endpoints enabled (GET /api/users/me/places, PUT|DELETE /api/users/me/places/{kind})")
}

// savedPlacesAdapter maps *store.SavedPlacesRepo onto the consumer-site
// interface internal/telemetry declares, so that package never imports
// internal/store. The two record types are field-identical; the conversion
// happens here at the boundary, as it does for the account-deletion tally.
type savedPlacesAdapter struct {
	repo *store.SavedPlacesRepo
}

func (a *savedPlacesAdapter) ListSavedPlaces(ctx context.Context, userID string) ([]telemetry.SavedPlaceData, error) {
	rows, err := a.repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Non-nil zero-length: the handler renders [] for an account that has
	// saved nothing, and a nil slice here would still marshal as [] only
	// because the handler rebuilds it — keeping it non-nil makes that
	// independent of the handler's implementation.
	places := make([]telemetry.SavedPlaceData, 0, len(rows))
	for _, r := range rows {
		places = append(places, toSavedPlaceData(r))
	}
	return places, nil
}

func (a *savedPlacesAdapter) UpsertSavedPlace(ctx context.Context, userID string, place telemetry.SavedPlaceData) (telemetry.SavedPlaceData, error) {
	stored, err := a.repo.Upsert(ctx, userID, store.SavedPlace{
		Kind:      store.SavedPlaceKind(place.Kind),
		Label:     place.Label,
		Latitude:  place.Latitude,
		Longitude: place.Longitude,
	})
	if err != nil {
		return telemetry.SavedPlaceData{}, err
	}
	return toSavedPlaceData(stored), nil
}

func (a *savedPlacesAdapter) DeleteSavedPlace(ctx context.Context, userID, kind string) (bool, error) {
	return a.repo.Delete(ctx, userID, store.SavedPlaceKind(kind))
}

// toSavedPlaceData converts one store record to the telemetry-layer shape.
// Written out field by field rather than as a struct conversion because the
// Kind types differ (store.SavedPlaceKind vs string), which is exactly the
// distinction that keeps the enum's closed set inside the store package.
func toSavedPlaceData(r store.SavedPlace) telemetry.SavedPlaceData {
	return telemetry.SavedPlaceData{
		Kind:      string(r.Kind),
		Label:     r.Label,
		Latitude:  r.Latitude,
		Longitude: r.Longitude,
	}
}
