package commands

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Scope is a Tesla Fleet API OAuth scope. Vehicle commands are gated on
// the scope granted by the vehicle owner at Tesla-account link time.
//
// Fleet API defines exactly two command scopes (developer.tesla.com Fleet
// API → Authentication → Overview): vehicle_cmds and vehicle_charging_cmds.
// There is NO "vehicle_remote_start" Fleet scope — that name belongs to the
// legacy Owner API. Fleet folds remote_start_drive into vehicle_cmds.
type Scope string

const (
	// ScopeVehicleCmds covers actuation: lock/unlock, climate, trunk, horn,
	// lights, remote start, and navigation.
	ScopeVehicleCmds Scope = "vehicle_cmds"
	// ScopeChargingCmds covers charging management: start/stop, charge limit,
	// charge port, and schedules.
	ScopeChargingCmds Scope = "vehicle_charging_cmds"
)

// ScopeSet is the set of scopes granted by a Tesla OAuth access token.
// Known reports whether the granting scopes could be determined at all: a
// token whose scopes cannot be parsed yields an unknown set, and the
// executor then defers the decision to Tesla rather than pre-denying.
type ScopeSet struct {
	known bool
	set   map[Scope]struct{}
}

// Known reports whether the token's scope claim was parseable.
func (s ScopeSet) Known() bool { return s.known }

// Has reports whether sc was granted. Always false on an unknown set.
func (s ScopeSet) Has(sc Scope) bool {
	_, ok := s.set[sc]
	return ok
}

// ParseScopes extracts the granted scope set from a Tesla Fleet OAuth
// access token. Fleet access tokens are JWTs whose payload carries the
// granted scopes in the "scp" claim (a JSON array) or, on some issuers, a
// space-delimited "scope" string. If the token is not a parseable JWT or
// carries neither claim, the returned set is not Known and the caller
// should let Tesla enforce scope.
func ParseScopes(accessToken string) ScopeSet {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return ScopeSet{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ScopeSet{}
	}
	var claims struct {
		Scp   []string `json:"scp"`
		Scope string   `json:"scope"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ScopeSet{}
	}

	raw := claims.Scp
	if len(raw) == 0 && claims.Scope != "" {
		raw = strings.Fields(claims.Scope)
	}
	if len(raw) == 0 {
		return ScopeSet{}
	}

	set := make(map[Scope]struct{}, len(raw))
	for _, s := range raw {
		set[Scope(s)] = struct{}{}
	}
	return ScopeSet{known: true, set: set}
}
