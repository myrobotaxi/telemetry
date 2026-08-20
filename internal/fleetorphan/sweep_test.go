package fleetorphan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Every collaborator is faked; no database, no Tesla, no network.

const (
	vinA = "5YJSA1E26MF00A001"
	vinB = "5YJSA1E26MF00B002"
	vinC = "5YJSA1E26MF00C003"
)

type fakeStore struct {
	tombstones   []TombstonedOrphan
	tombstoneErr error

	linkedUsers   []string
	linkedErr     error
	vinsByUser    map[string][]string
	vinsByUserErr map[string]error

	orphanAttempts    []string
	orphanAttemptsErr error
	deleted           []string
	deleteErr         error
}

func (f *fakeStore) ListTombstonedOrphanVINs(_ context.Context, _ int) ([]TombstonedOrphan, error) {
	return f.tombstones, f.tombstoneErr
}

func (f *fakeStore) ListTeslaLinkedUserIDs(_ context.Context) ([]string, error) {
	return f.linkedUsers, f.linkedErr
}

func (f *fakeStore) ListVehicleVINsByUser(_ context.Context, userID string) ([]string, error) {
	if err, ok := f.vinsByUserErr[userID]; ok {
		return nil, err
	}
	return f.vinsByUser[userID], nil
}

func (f *fakeStore) ListOrphanFleetConfigAttempts(_ context.Context) ([]string, error) {
	return f.orphanAttempts, f.orphanAttemptsErr
}

func (f *fakeStore) DeleteFleetConfigAttempts(_ context.Context, ids []string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deleted = append(f.deleted, ids...)
	return int64(len(ids)), nil
}

type fakeFleet struct {
	byToken map[string][]FleetVehicle
	err     error
}

func (f *fakeFleet) ListVehicles(_ context.Context, token string) ([]FleetVehicle, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byToken[token], nil
}

type fakeTesla struct {
	status    map[string]ConfigStatus
	statusErr map[string]error
	deleteErr map[string]error
	deletes   []string
}

func (f *fakeTesla) GetTelemetryConfig(_ context.Context, _, vin string) (ConfigStatus, error) {
	if err, ok := f.statusErr[vin]; ok {
		return ConfigStatus{}, err
	}
	return f.status[vin], nil
}

func (f *fakeTesla) DeleteTelemetryConfig(_ context.Context, _, vin string) error {
	if err, ok := f.deleteErr[vin]; ok {
		return err
	}
	f.deletes = append(f.deletes, vin)
	return nil
}

type fakeTokens struct {
	byUser map[string]string
	errs   map[string]error
	calls  map[string]int
}

func (f *fakeTokens) AccessToken(_ context.Context, userID string) (string, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[userID]++
	if err, ok := f.errs[userID]; ok {
		return "", err
	}
	tok, ok := f.byUser[userID]
	if !ok {
		return "", fmt.Errorf("%w (user %s)", ErrNoToken, userID)
	}
	return tok, nil
}

// outcomeFor finds one VIN's line in the report.
func outcomeFor(t *testing.T, rep Report, vin string) VINOutcome {
	t.Helper()
	for _, o := range rep.VINs {
		if o.VIN == vin {
			return o
		}
	}
	t.Fatalf("no report line for %s (have %+v)", vin, rep.VINs)
	return VINOutcome{}
}

// The five labels, one case each, driven through the whole Run.
func TestRunLabelsEachVIN(t *testing.T) {
	tests := []struct {
		name    string
		apply   bool
		build   func() (*fakeStore, *fakeTesla, *fakeTokens)
		vin     string
		want    string
		wantSub string // substring the Detail must contain, "" to skip
	}{
		{
			name: "configured tombstoned vin on a dry run is would_delete",
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{status: map[string]ConfigStatus{vinA: {Configured: true}}},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:  vinA,
			want: OutcomeWouldDelete,
		},
		{
			name:  "configured tombstoned vin under apply is deleted",
			apply: true,
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{status: map[string]ConfigStatus{vinA: {Configured: true}}},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:  vinA,
			want: OutcomeDeleted,
		},
		{
			name: "tesla reports nothing configured",
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{status: map[string]ConfigStatus{vinA: {Configured: false}}},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:  vinA,
			want: OutcomeNoConfig,
		},
		{
			name: "a 404 from tesla reads as no_config, not a failure",
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{statusErr: map[string]error{vinA: ErrNoConfig}},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:  vinA,
			want: OutcomeNoConfig,
		},
		{
			name: "a deleted account leaves the config unreachable",
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "gone", VIN: vinA}}},
					&fakeTesla{status: map[string]ConfigStatus{vinA: {Configured: true}}},
					&fakeTokens{} // no token for anyone
			},
			vin:     vinA,
			want:    OutcomeUnreachableNoToken,
			wantSub: "exp",
		},
		{
			name: "a tesla read error is failed and carries the reason",
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{statusErr: map[string]error{vinA: errors.New("fleet API error 500: boom")}},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:     vinA,
			want:    OutcomeFailed,
			wantSub: "read config",
		},
		{
			name:  "a delete error is failed, and the config is left alone",
			apply: true,
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{
						status:    map[string]ConfigStatus{vinA: {Configured: true}},
						deleteErr: map[string]error{vinA: errors.New("fleet API error 403: forbidden")},
					},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:     vinA,
			want:    OutcomeFailed,
			wantSub: "delete config",
		},
		{
			name:  "a delete that races another remover settles as no_config",
			apply: true,
			build: func() (*fakeStore, *fakeTesla, *fakeTokens) {
				return &fakeStore{tombstones: []TombstonedOrphan{{UserID: "u1", VIN: vinA}}},
					&fakeTesla{
						status:    map[string]ConfigStatus{vinA: {Configured: true}},
						deleteErr: map[string]error{vinA: ErrNoConfig},
					},
					&fakeTokens{byUser: map[string]string{"u1": "tok1"}}
			},
			vin:  vinA,
			want: OutcomeNoConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, tesla, tokens := tt.build()
			s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: tesla, Deleter: tesla, Tokens: tokens},
				Config{Apply: tt.apply}, nil)

			rep, err := s.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := outcomeFor(t, rep, tt.vin)
			if got.Outcome != tt.want {
				t.Fatalf("outcome = %q, want %q (detail %q)", got.Outcome, tt.want, got.Detail)
			}
			if tt.wantSub != "" && !strings.Contains(got.Detail, tt.wantSub) {
				t.Fatalf("detail = %q, want it to mention %q", got.Detail, tt.wantSub)
			}
			if rep.Counts[tt.want] != 1 {
				t.Fatalf("counts[%s] = %d, want 1 (counts %v)", tt.want, rep.Counts[tt.want], rep.Counts)
			}
			if rep.DryRun == tt.apply {
				t.Fatalf("DryRun = %v with Apply = %v", rep.DryRun, tt.apply)
			}
		})
	}
}

// A dry run must make ZERO writes and ZERO Tesla deletes. This is the property
// the command's default depends on.
func TestDryRunWritesNothing(t *testing.T) {
	st := &fakeStore{
		tombstones:     []TombstonedOrphan{{UserID: "u1", VIN: vinA}},
		linkedUsers:    []string{"u1"},
		vinsByUser:     map[string][]string{"u1": nil},
		orphanAttempts: []string{"veh_dead_1", "veh_dead_2"},
	}
	tesla := &fakeTesla{status: map[string]ConfigStatus{
		vinA: {Configured: true},
		vinB: {Configured: true},
	}}
	fleet := &fakeFleet{byToken: map[string][]FleetVehicle{"tok1": {{VIN: vinB, Owner: true}}}}
	tokens := &fakeTokens{byUser: map[string]string{"u1": "tok1"}}

	s := New(Deps{Store: st, Fleet: fleet, Reader: tesla, Deleter: tesla, Tokens: tokens}, Config{}, nil)
	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(tesla.deletes) != 0 {
		t.Fatalf("dry run issued Tesla deletes: %v", tesla.deletes)
	}
	if len(st.deleted) != 0 {
		t.Fatalf("dry run issued DB deletes: %v", st.deleted)
	}
	if rep.Attempts.Deleted != 0 {
		t.Fatalf("Attempts.Deleted = %d on a dry run", rep.Attempts.Deleted)
	}
	if len(rep.Attempts.OrphanRows) != 2 {
		t.Fatalf("OrphanRows = %v, want both reported even on a dry run", rep.Attempts.OrphanRows)
	}
	if rep.Counts[OutcomeWouldDelete] != 2 {
		t.Fatalf("would_delete = %d, want 2 (counts %v)", rep.Counts[OutcomeWouldDelete], rep.Counts)
	}
}

func TestApplyClearsOrphanAttemptRows(t *testing.T) {
	st := &fakeStore{orphanAttempts: []string{"veh_dead_1", "veh_dead_2"}}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: &fakeTokens{}},
		Config{Apply: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Attempts.Deleted != 2 || len(st.deleted) != 2 {
		t.Fatalf("deleted %d rows (store saw %v), want 2", rep.Attempts.Deleted, st.deleted)
	}
}

// Source C needs no token, so it must still run when every Tesla path is dead.
// That is the guarantee that makes the command useful against a fleet of
// deleted accounts.
func TestAttemptRowsAreSweptEvenWhenTeslaIsUnreachable(t *testing.T) {
	st := &fakeStore{
		tombstones:     []TombstonedOrphan{{UserID: "gone", VIN: vinA}},
		linkedUsers:    []string{"u1"},
		orphanAttempts: []string{"veh_dead_1"},
	}
	tokens := &fakeTokens{errs: map[string]error{"u1": errors.New("db down")}}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: tokens},
		Config{Apply: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Attempts.Deleted != 1 {
		t.Fatalf("Attempts.Deleted = %d, want 1", rep.Attempts.Deleted)
	}
	if outcomeFor(t, rep, vinA).Outcome != OutcomeUnreachableNoToken {
		t.Fatalf("vinA = %+v, want unreachable_no_token", outcomeFor(t, rep, vinA))
	}
	if len(rep.Errors) == 0 {
		t.Fatal("a failed token resolution for a linked user must be reported")
	}
}

// -apply with no deleter wired would report deletions that never happened.
func TestApplyWithoutADeleterDowngradesToADryRun(t *testing.T) {
	st := &fakeStore{
		tombstones:     []TombstonedOrphan{{UserID: "u1", VIN: vinA}},
		orphanAttempts: []string{"veh_dead_1"},
	}
	tesla := &fakeTesla{status: map[string]ConfigStatus{vinA: {Configured: true}}}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: tesla, Tokens: &fakeTokens{byUser: map[string]string{"u1": "tok1"}}},
		Config{Apply: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.DryRun {
		t.Fatal("DryRun = false with no deleter wired")
	}
	if outcomeFor(t, rep, vinA).Outcome != OutcomeWouldDelete {
		t.Fatalf("outcome = %q, want would_delete", outcomeFor(t, rep, vinA).Outcome)
	}
	if len(st.deleted) != 0 {
		t.Fatalf("the DB half wrote anyway: %v", st.deleted)
	}
}

func TestRunRequiresAStore(t *testing.T) {
	if _, err := New(Deps{}, Config{}, nil).Run(context.Background()); err == nil {
		t.Fatal("Run with no Store must error")
	}
}
