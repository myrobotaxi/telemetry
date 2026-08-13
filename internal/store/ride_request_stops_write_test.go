package store

import (
	"errors"
	"testing"
)

// MYR-539 — the REPLACEMENT rules for a ride's stop list. These are the rules a
// wrong answer would show up as a car driving somewhere the trip has already
// been, so they are tested against the planner directly: it is a pure function
// precisely so the argument can be read without a database in the way.

// stop builds one existing stop at its list position.
func stop(id string, status RideStopStatus) RideStop {
	return RideStop{ID: id, Status: status, Place: RidePlace{Latitude: 1, Longitude: 2, Label: id}}
}

// keep/add build the two kinds of submitted entry.
func keep(id string) RideStopEdit {
	return RideStopEdit{ID: id, Place: RidePlace{Latitude: 3, Longitude: 4, Label: id + " moved"}}
}
func add(label string) RideStopEdit {
	return RideStopEdit{Place: RidePlace{Latitude: 5, Longitude: 6, Label: label}}
}

func TestPlanStopReplacement(t *testing.T) {
	tests := []struct {
		name     string
		existing []RideStop
		desired  []RideStopEdit
		wantErr  error
		// wantWrites is the (id, position, frozen) triple of each planned
		// write, in order; an empty id means a newly minted stop.
		wantWrites  []stopWrite
		wantDeletes []string
	}{
		{
			name:     "first stops on a plain trip are all inserts",
			existing: nil,
			desired:  []RideStopEdit{add("Bakery"), add("Pharmacy")},
			wantWrites: []stopWrite{
				{Position: 0}, {Position: 1},
			},
		},
		{
			name:     "an omitted upcoming stop is removed",
			existing: []RideStop{stop("a", RideStopUpcoming), stop("b", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("b")},
			wantWrites: []stopWrite{
				{ID: "b", Position: 0},
			},
			wantDeletes: []string{"a"},
		},
		{
			name:     "upcoming stops reorder freely",
			existing: []RideStop{stop("a", RideStopUpcoming), stop("b", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("b"), keep("a")},
			wantWrites: []stopWrite{
				{ID: "b", Position: 0}, {ID: "a", Position: 1},
			},
		},
		{
			name:     "a new stop may be inserted after the current one",
			existing: []RideStop{stop("a", RideStopCompleted), stop("b", RideStopCurrent), stop("c", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("a"), keep("b"), add("Detour"), keep("c")},
			wantWrites: []stopWrite{
				{ID: "a", Position: 0, Frozen: true},
				{ID: "b", Position: 1, Frozen: true},
				{Position: 2},
				{ID: "c", Position: 3},
			},
		},
		{
			// The road behind the car cannot be rewritten.
			name:     "removing a completed stop is refused",
			existing: []RideStop{stop("a", RideStopCompleted), stop("b", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("b")},
			wantErr:  ErrRideRequestConflict,
		},
		{
			name:     "removing the current stop is refused",
			existing: []RideStop{stop("a", RideStopCurrent)},
			desired:  []RideStopEdit{},
			wantErr:  ErrRideRequestConflict,
		},
		{
			name:     "reordering a frozen stop is refused",
			existing: []RideStop{stop("a", RideStopCompleted), stop("b", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("b"), keep("a")},
			wantErr:  ErrRideRequestConflict,
		},
		{
			// Inserting AHEAD of a completed stop renumbers the road already
			// travelled, which is the same lie as reordering it.
			name:     "inserting before a completed stop is refused",
			existing: []RideStop{stop("a", RideStopCompleted)},
			desired:  []RideStopEdit{add("Detour"), keep("a")},
			wantErr:  ErrRideRequestConflict,
		},
		{
			name:     "an id the ride does not have is a bad request",
			existing: []RideStop{stop("a", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("ghost")},
			wantErr:  ErrRideStopUnknown,
		},
		{
			name:     "the same id twice is a bad request",
			existing: []RideStop{stop("a", RideStopUpcoming)},
			desired:  []RideStopEdit{keep("a"), keep("a")},
			wantErr:  ErrRideStopUnknown,
		},
		{
			name:        "an empty list clears every upcoming stop",
			existing:    []RideStop{stop("a", RideStopUpcoming), stop("b", RideStopUpcoming)},
			desired:     []RideStopEdit{},
			wantWrites:  []stopWrite{},
			wantDeletes: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planStopReplacement(tt.existing, tt.desired)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(plan.Writes) != len(tt.wantWrites) {
				t.Fatalf("writes = %d, want %d (%+v)", len(plan.Writes), len(tt.wantWrites), plan.Writes)
			}
			for i, want := range tt.wantWrites {
				got := plan.Writes[i]
				if got.ID != want.ID || got.Position != want.Position || got.Frozen != want.Frozen {
					t.Errorf("write %d = {id:%q pos:%d frozen:%v}, want {id:%q pos:%d frozen:%v}",
						i, got.ID, got.Position, got.Frozen, want.ID, want.Position, want.Frozen)
				}
			}
			if len(plan.Deletes) != len(tt.wantDeletes) {
				t.Fatalf("deletes = %v, want %v", plan.Deletes, tt.wantDeletes)
			}
			for i, want := range tt.wantDeletes {
				if plan.Deletes[i] != want {
					t.Errorf("delete %d = %q, want %q", i, plan.Deletes[i], want)
				}
			}
		})
	}
}

// A frozen stop's PLACE is not writable, and the plan says so on the write
// rather than refusing the edit: the client restates every entry's place by
// construction, so a byte-difference on a stop nobody is editing must not
// refuse the rest of the trip.
func TestPlanStopReplacement_FrozenPlaceIsNotWritten(t *testing.T) {
	existing := []RideStop{stop("a", RideStopCurrent), stop("b", RideStopUpcoming)}
	plan, err := planStopReplacement(existing, []RideStopEdit{keep("a"), keep("b")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !plan.Writes[0].Frozen {
		t.Error("the current stop's write must be marked frozen so its place is preserved")
	}
	if plan.Writes[1].Frozen {
		t.Error("an upcoming stop's place must be writable")
	}
}
