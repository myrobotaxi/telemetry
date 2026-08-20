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

	// MYR-596 legacy backlog. orphanTombstones is the count the predicate
	// matches; purgedTombstones records that the DELETE actually ran, which is
	// what separates "counted on a dry run" from "cleared under -apply".
	orphanTombstones    int64
	orphanTombstonesErr error
	purgedTombstones    int
	purgeTombstonesErr  error
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

func (f *fakeStore) CountOrphanedTombstones(_ context.Context) (int64, error) {
	return f.orphanTombstones, f.orphanTombstonesErr
}

func (f *fakeStore) PurgeOrphanedTombstones(_ context.Context) (int64, error) {
	if f.purgeTombstonesErr != nil {
		return 0, f.purgeTombstonesErr
	}
	f.purgedTombstones++
	n := f.orphanTombstones
	f.orphanTombstones = 0 // idempotent: a re-run finds none
	return n, nil
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

// --- MYR-596 legacy tombstone backlog --------------------------------------

// The purge is OPT-IN. -apply alone must not touch go_removed_vehicles: those
// rows are source A, and destroying an operator's only handle on a deleted
// owner's VINs is not something the ordinary cleanup run should do silently.
func TestOrphanTombstonesAreUntouchedWithoutTheFlag(t *testing.T) {
	st := &fakeStore{orphanTombstones: 7, orphanAttempts: []string{"veh_dead_1"}}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: &fakeTokens{}},
		Config{Apply: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Tombstones.Requested {
		t.Fatal("Tombstones.Requested = true without -purge-orphan-tombstones")
	}
	if st.purgedTombstones != 0 {
		t.Fatalf("the purge ran %d time(s) unasked", st.purgedTombstones)
	}
	if rep.Tombstones.Orphaned != 0 || rep.Tombstones.Deleted != 0 {
		t.Fatalf("Tombstones = %+v, want zeroes when not requested", rep.Tombstones)
	}
}

// Requested but not applied: count the backlog, write nothing. This is the run
// an operator should do first, because the JSON it produces is the last record
// of those VINs that will ever exist.
func TestOrphanTombstonesAreOnlyCountedOnADryRun(t *testing.T) {
	st := &fakeStore{orphanTombstones: 7}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: &fakeTokens{}},
		Config{PurgeOrphanTombstones: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Tombstones.Requested || rep.Tombstones.Orphaned != 7 {
		t.Fatalf("Tombstones = %+v, want requested with 7 orphans", rep.Tombstones)
	}
	if rep.Tombstones.Deleted != 0 || st.purgedTombstones != 0 {
		t.Fatalf("a dry run deleted %d rows (store purges: %d)", rep.Tombstones.Deleted, st.purgedTombstones)
	}
}

// Under -apply the backlog goes, and a re-run finds nothing — the idempotency
// every step of this program depends on.
func TestApplyWithTheFlagClearsTheTombstoneBacklog(t *testing.T) {
	st := &fakeStore{orphanTombstones: 7}
	deps := Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: &fakeTokens{}}
	cfg := Config{Apply: true, PurgeOrphanTombstones: true}

	rep, err := New(deps, cfg, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Tombstones.Orphaned != 7 || rep.Tombstones.Deleted != 7 {
		t.Fatalf("Tombstones = %+v, want 7 found and 7 deleted", rep.Tombstones)
	}

	again, err := New(deps, cfg, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again.Tombstones.Orphaned != 0 || again.Tombstones.Deleted != 0 {
		t.Fatalf("re-run Tombstones = %+v, want zeroes", again.Tombstones)
	}
}

// The purge runs LAST, so a report that names a VIN is produced before the row
// naming it is removed. Pinned by asserting the VIN pass still populated the
// report in the very run that emptied the table.
func TestTombstonePurgeRunsAfterTheVINPass(t *testing.T) {
	st := &fakeStore{
		tombstones:       []TombstonedOrphan{{UserID: "gone", VIN: vinA}},
		orphanTombstones: 1,
	}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: &fakeTokens{}},
		Config{Apply: true, PurgeOrphanTombstones: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Tombstones.Deleted != 1 {
		t.Fatalf("Tombstones.Deleted = %d, want 1", rep.Tombstones.Deleted)
	}
	// The whole point: the VIN is still IN the report the purge ran inside of.
	if outcomeFor(t, rep, vinA).Outcome != OutcomeUnreachableNoToken {
		t.Fatalf("vinA = %+v, want it reported before its tombstone went", outcomeFor(t, rep, vinA))
	}
}

// A purge failure is a report line, never a fatal — the VIN work above it is
// worth returning whatever the database did.
func TestTombstonePurgeFailureIsReportedNotFatal(t *testing.T) {
	st := &fakeStore{orphanTombstones: 3, purgeTombstonesErr: errors.New("db down")}
	s := New(Deps{Store: st, Fleet: &fakeFleet{}, Reader: &fakeTesla{}, Deleter: &fakeTesla{}, Tokens: &fakeTokens{}},
		Config{Apply: true, PurgeOrphanTombstones: true}, nil)

	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run must not fail on a purge error: %v", err)
	}
	if rep.Tombstones.Deleted != 0 {
		t.Fatalf("Tombstones.Deleted = %d after a failed purge", rep.Tombstones.Deleted)
	}
	if len(rep.Errors) == 0 {
		t.Fatal("a failed purge must be reported")
	}
}

func TestRunRequiresAStore(t *testing.T) {
	if _, err := New(Deps{}, Config{}, nil).Run(context.Background()); err == nil {
		t.Fatal("Run with no Store must error")
	}
}
