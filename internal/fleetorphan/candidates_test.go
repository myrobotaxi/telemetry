package fleetorphan

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Candidate collection is where a mistake deletes a live car's config, so each
// rule gets its own case.
func TestCollectCandidates(t *testing.T) {
	tests := []struct {
		name      string
		store     *fakeStore
		fleet     *fakeFleet
		tokens    *fakeTokens
		wantVINs  []string
		wantOwner map[string]string // vin -> the handle the sweep will use
		wantErrs  int
	}{
		{
			name: "a tesla-side vin we hold no row for is a candidate",
			store: &fakeStore{
				linkedUsers: []string{"u1"},
				vinsByUser:  map[string][]string{"u1": {vinA}},
			},
			fleet: &fakeFleet{byToken: map[string][]FleetVehicle{
				"tok1": {{VIN: vinA, Owner: true}, {VIN: vinB, Owner: true}},
			}},
			tokens:   &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
			wantVINs: []string{vinB},
		},
		{
			name: "a shared-driver entry is never a candidate",
			store: &fakeStore{
				linkedUsers: []string{"u1"},
				vinsByUser:  map[string][]string{"u1": nil},
			},
			fleet: &fakeFleet{byToken: map[string][]FleetVehicle{
				"tok1": {{VIN: vinB, Owner: false}},
			}},
			tokens:   &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
			wantVINs: nil,
		},
		{
			name: "an unreadable local vin set skips the owner entirely",
			store: &fakeStore{
				linkedUsers:   []string{"u1"},
				vinsByUserErr: map[string]error{"u1": errors.New("db down")},
			},
			fleet: &fakeFleet{byToken: map[string][]FleetVehicle{
				"tok1": {{VIN: vinA, Owner: true}, {VIN: vinB, Owner: true}},
			}},
			tokens:   &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
			wantVINs: nil,
			wantErrs: 1,
		},
		{
			name: "a failed fleet list is an error, not a candidate",
			store: &fakeStore{
				linkedUsers: []string{"u1"},
				vinsByUser:  map[string][]string{"u1": nil},
			},
			fleet:    &fakeFleet{err: errors.New("fleet API error 429")},
			tokens:   &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
			wantVINs: nil,
			wantErrs: 1,
		},
		{
			name: "sources union, and the live owner wins the token handle",
			store: &fakeStore{
				// The tombstone remembers a now-deleted account.
				tombstones:  []TombstonedOrphan{{UserID: "gone", VIN: vinB}},
				linkedUsers: []string{"u1"},
				vinsByUser:  map[string][]string{"u1": nil},
			},
			fleet: &fakeFleet{byToken: map[string][]FleetVehicle{
				"tok1": {{VIN: vinB, Owner: true}},
			}},
			tokens:    &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
			wantVINs:  []string{vinB},
			wantOwner: map[string]string{vinB: "u1"},
		},
		{
			name: "candidates are vin-sorted regardless of source order",
			store: &fakeStore{
				tombstones:  []TombstonedOrphan{{UserID: "gone", VIN: vinC}},
				linkedUsers: []string{"u1"},
				vinsByUser:  map[string][]string{"u1": nil},
			},
			fleet: &fakeFleet{byToken: map[string][]FleetVehicle{
				"tok1": {{VIN: vinB, Owner: true}, {VIN: vinA, Owner: true}},
			}},
			tokens:   &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
			wantVINs: []string{vinA, vinB, vinC},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Deps{Store: tt.store, Fleet: tt.fleet, Tokens: tt.tokens}, Config{}, nil)
			rep := newReport(true)
			got := s.collect(context.Background(), &rep)

			if len(got) != len(tt.wantVINs) {
				t.Fatalf("candidates = %+v, want VINs %v", got, tt.wantVINs)
			}
			for i, c := range got {
				if c.VIN != tt.wantVINs[i] {
					t.Fatalf("candidate[%d].VIN = %s, want %s", i, c.VIN, tt.wantVINs[i])
				}
				if want, ok := tt.wantOwner[c.VIN]; ok && c.UserID != want {
					t.Fatalf("candidate %s owner = %s, want %s", c.VIN, c.UserID, want)
				}
			}
			if len(rep.Errors) != tt.wantErrs {
				t.Fatalf("errors = %v, want %d", rep.Errors, tt.wantErrs)
			}
		})
	}
}

// The union case above should carry BOTH source labels through to the report.
func TestSourcesAreUnioned(t *testing.T) {
	st := &fakeStore{
		tombstones:  []TombstonedOrphan{{UserID: "gone", VIN: vinB}},
		linkedUsers: []string{"u1"},
		vinsByUser:  map[string][]string{"u1": nil},
	}
	s := New(Deps{
		Store:  st,
		Fleet:  &fakeFleet{byToken: map[string][]FleetVehicle{"tok1": {{VIN: vinB, Owner: true}}}},
		Tokens: &fakeTokens{byUser: map[string]string{"u1": "tok1"}},
	}, Config{}, nil)

	rep := newReport(true)
	got := s.collect(context.Background(), &rep)
	if len(got) != 1 {
		t.Fatalf("candidates = %+v, want 1", got)
	}
	if strings.Join(got[0].Sources, ",") != SourceTombstone+","+SourceFleetList {
		t.Fatalf("sources = %v, want [tombstone fleet_list] in that order", got[0].Sources)
	}
}

// A token is resolved once per owner per run: source B already paid for it, and
// the per-VIN pass must not trigger a second OAuth refresh for every car.
func TestTokenResolutionIsMemoisedPerRun(t *testing.T) {
	st := &fakeStore{
		linkedUsers: []string{"u1"},
		vinsByUser:  map[string][]string{"u1": nil},
	}
	tesla := &fakeTesla{status: map[string]ConfigStatus{
		vinA: {Configured: false},
		vinB: {Configured: false},
	}}
	tokens := &fakeTokens{byUser: map[string]string{"u1": "tok1"}}
	s := New(Deps{
		Store:  st,
		Fleet:  &fakeFleet{byToken: map[string][]FleetVehicle{"tok1": {{VIN: vinA, Owner: true}, {VIN: vinB, Owner: true}}}},
		Reader: tesla,
		Tokens: tokens,
	}, Config{}, nil)

	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tokens.calls["u1"] != 1 {
		t.Fatalf("resolved the token %d times for one owner across two VINs, want 1", tokens.calls["u1"])
	}
}

// A no-token owner is asked once, not once per orphaned VIN.
func TestMissingTokenIsCachedToo(t *testing.T) {
	st := &fakeStore{tombstones: []TombstonedOrphan{
		{UserID: "gone", VIN: vinA},
		{UserID: "gone", VIN: vinB},
	}}
	tokens := &fakeTokens{}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Tokens: tokens}, Config{}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Counts[OutcomeUnreachableNoToken] != 2 {
		t.Fatalf("unreachable = %d, want 2", rep.Counts[OutcomeUnreachableNoToken])
	}
	if tokens.calls["gone"] != 1 {
		t.Fatalf("asked the token source %d times for a tokenless owner, want 1", tokens.calls["gone"])
	}
}

// An empty token is as good as no token everywhere downstream, so it must be
// labelled that way and not sent to Tesla.
func TestEmptyTokenReadsAsNoToken(t *testing.T) {
	st := &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}}
	tesla := &fakeTesla{status: map[string]ConfigStatus{vinA: {Configured: true}}}
	s := New(Deps{
		Store:  st,
		Fleet:  &fakeFleet{},
		Reader: tesla,
		Tokens: &fakeTokens{byUser: map[string]string{"u1": ""}},
	}, Config{}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := outcomeFor(t, rep, vinA).Outcome; got != OutcomeUnreachableNoToken {
		t.Fatalf("outcome = %q, want unreachable_no_token", got)
	}
}

func TestRedactVIN(t *testing.T) {
	tests := []struct{ in, want string }{
		{vinA, "***A001"},
		{"abcd", "abcd"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := redactVIN(tt.in); got != tt.want {
				t.Fatalf("redactVIN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
