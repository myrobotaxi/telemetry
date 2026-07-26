package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// dropoffReconcileStore is an OutcomeStore that records LEG-2 (dropoff)
// outcomes into its own map and FAILS the test if the leg-1 record seam is
// ever touched — so a passing ReconcileDropoff test proves the leg-2 path
// writes the independent dropoff_* columns, never leg 1's.
type dropoffReconcileStore struct {
	mu           sync.Mutex
	dropRecorded map[string]recordCall
	failIDs      map[string]bool
	leg1Touched  bool
}

func (s *dropoffReconcileStore) ClaimDispatch(context.Context, string) (bool, error) {
	return true, nil
}

func (s *dropoffReconcileStore) RecordDispatchOutcome(context.Context, string, Outcome, *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leg1Touched = true
	return nil
}

func (s *dropoffReconcileStore) ClaimDropoffDispatch(context.Context, string) (bool, error) {
	return true, nil
}

func (s *dropoffReconcileStore) RecordDropoffDispatchOutcome(_ context.Context, id string, status Outcome, code *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failIDs[id] {
		return errors.New("dropoff record failed for " + id)
	}
	rc := recordCall{status: status}
	if code != nil {
		rc.code = *code
	}
	s.dropRecorded[id] = rc
	return nil
}

// fakeDropoffLister feeds ReconcileDropoff a scripted id list and records the
// olderThan it was asked for.
type fakeDropoffLister struct {
	ids          []string
	err          error
	gotOlderThan time.Duration
	called       bool
}

func (f *fakeDropoffLister) ListInterruptedDropoffDispatches(_ context.Context, olderThan time.Duration) ([]string, error) {
	f.called = true
	f.gotOlderThan = olderThan
	return f.ids, f.err
}

// TestReconcileDropoff mirrors TestReconcile for leg 2 (MYR-266): interrupted
// dropoff dispatches are resolved failed/dispatch_interrupted via the dropoff
// seam; a list error propagates and records nothing; a record failure is
// skipped without aborting the pass; and leg 1 is never touched.
func TestReconcileDropoff(t *testing.T) {
	tests := []struct {
		name         string
		ids          []string
		listErr      error
		failIDs      map[string]bool
		wantResolved int
		wantErr      bool
		wantRecorded []string
	}{
		{
			name:         "resolves all interrupted dropoffs",
			ids:          []string{"r1", "r2", "r3"},
			wantResolved: 3,
			wantRecorded: []string{"r1", "r2", "r3"},
		},
		{
			name:         "no interrupted dropoffs",
			ids:          nil,
			wantResolved: 0,
		},
		{
			name:    "list error propagates, nothing recorded",
			listErr: errors.New("db down"),
			wantErr: true,
		},
		{
			name:         "continues past a record failure",
			ids:          []string{"r1", "r2", "r3"},
			failIDs:      map[string]bool{"r2": true},
			wantResolved: 2,
			wantRecorded: []string{"r1", "r3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &dropoffReconcileStore{dropRecorded: map[string]recordCall{}, failIDs: tt.failIDs}
			d := New(&fakeVehicleResolver{}, &fakeTokenSource{}, &fakeExecutor{}, st,
				Config{Enabled: true}, nil)
			lister := &fakeDropoffLister{ids: tt.ids, err: tt.listErr}

			n, err := d.ReconcileDropoff(context.Background(), lister, time.Minute)

			if tt.wantErr {
				if err == nil {
					t.Fatal("ReconcileDropoff expected error, got nil")
				}
				if len(st.dropRecorded) != 0 {
					t.Errorf("recorded %+v on list error, want none", st.dropRecorded)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReconcileDropoff: %v", err)
			}
			if n != tt.wantResolved {
				t.Errorf("resolved = %d, want %d", n, tt.wantResolved)
			}
			if st.leg1Touched {
				t.Error("leg-1 record seam touched by a dropoff reconcile")
			}
			for _, id := range tt.wantRecorded {
				rc, ok := st.dropRecorded[id]
				if !ok {
					t.Errorf("dropoff %s not recorded", id)
					continue
				}
				if rc.status != OutcomeFailed || rc.code != codeDispatchInterrupted {
					t.Errorf("dropoff %s recorded %+v, want {failed, dispatch_interrupted}", id, rc)
				}
			}
		})
	}
}

// TestReconcileDropoff_FloorsOlderThanAtOverallTimeout proves the leg-2 age
// floor matches leg 1: a cutoff below OverallTimeout is raised so a live
// in-flight dropoff (dropoff_dispatched_at set, status NULL) is never mistaken
// for an orphan.
func TestReconcileDropoff_FloorsOlderThanAtOverallTimeout(t *testing.T) {
	st := &dropoffReconcileStore{dropRecorded: map[string]recordCall{}}
	d := New(&fakeVehicleResolver{}, &fakeTokenSource{}, &fakeExecutor{}, st,
		Config{Enabled: true, OverallTimeout: 90 * time.Second}, nil)
	lister := &fakeDropoffLister{}

	if _, err := d.ReconcileDropoff(context.Background(), lister, time.Second); err != nil {
		t.Fatalf("ReconcileDropoff: %v", err)
	}
	if !lister.called {
		t.Fatal("dropoff lister was not called")
	}
	if lister.gotOlderThan != 90*time.Second {
		t.Errorf("olderThan = %v, want floored to 90s (OverallTimeout)", lister.gotOlderThan)
	}
}
