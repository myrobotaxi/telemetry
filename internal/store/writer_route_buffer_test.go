package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"
)

// --- test doubles -----------------------------------------------------

// recordedRouteFlush is one observed AppendRoutePoints call. The points
// slice is a COPY: flushDrive re-buffers with
// `append(pts, rb.buffers[driveID]...)`, which writes through the same
// backing array it just handed to the persister, so a retained slice
// header would silently mutate under the assertions.
type recordedRouteFlush struct {
	driveID string
	points  []RoutePointRecord
}

// fakeRoutePersister is a drivePersister that records AppendRoutePoints
// calls and returns a scripted error per call, so a test can express
// "fail the first flush, succeed the second".
type fakeRoutePersister struct {
	mu    sync.Mutex
	calls []recordedRouteFlush

	// errs[i] is returned by the i-th AppendRoutePoints call. Calls past
	// the end of errs return defaultErr (nil unless set).
	errs       []error
	defaultErr error

	// onCall, when set, runs at the start of the i-th AppendRoutePoints
	// call, before any internal lock is taken. It exists so a test can
	// inject an `add` that races the in-flight flush — the buffer entry
	// has already been deleted at this point, exactly as in production.
	onCall func(call int)
}

// newFakeRoutePersister builds a persister whose i-th flush returns
// errs[i]; a nil entry (or any call beyond errs) succeeds.
func newFakeRoutePersister(t *testing.T, errs ...error) *fakeRoutePersister {
	t.Helper()
	return &fakeRoutePersister{errs: errs}
}

func (f *fakeRoutePersister) Create(_ context.Context, _ DriveRecord) error { return nil }

func (f *fakeRoutePersister) Complete(_ context.Context, _ string, _ DriveCompletion) error {
	return nil
}

func (f *fakeRoutePersister) Delete(_ context.Context, _ string) error { return nil }

func (f *fakeRoutePersister) AppendRoutePoints(_ context.Context, driveID string, points []RoutePointRecord) error {
	f.mu.Lock()
	n := len(f.calls)
	hook := f.onCall
	f.mu.Unlock()

	if hook != nil {
		hook(n)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedRouteFlush{
		driveID: driveID,
		points:  append([]RoutePointRecord(nil), points...),
	})
	if n < len(f.errs) {
		return f.errs[n]
	}
	return f.defaultErr
}

// snapshot returns a copy of the recorded calls, taken under the lock.
func (f *fakeRoutePersister) snapshot() []recordedRouteFlush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRouteFlush(nil), f.calls...)
}

// callCount reports how many AppendRoutePoints calls have been made.
func (f *fakeRoutePersister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- helpers ----------------------------------------------------------

// newRouteBufferForTest builds a routeBuffer wired to p with a logger
// that throws its output away. The flush goroutine is NOT started: every
// test below drives flushDrive/flushAll directly so no test depends on
// wall-clock timing.
func newRouteBufferForTest(t *testing.T, p drivePersister, flushSize int) *routeBuffer {
	t.Helper()
	return newRouteBuffer(p, slog.New(slog.NewTextHandler(io.Discard, nil)), RouteBufferConfig{
		FlushInterval: time.Hour,
		FlushSize:     flushSize,
	})
}

// routePoint returns a deterministic, uniquely identifiable point so
// ordering assertions can name the point they expected.
func routePoint(i int) RoutePointRecord {
	return RoutePointRecord{
		Latitude:  37.0 + float64(i),
		Longitude: -122.0 - float64(i),
		Speed:     float64(i),
		Heading:   float64(i % 360),
		Timestamp: fmt.Sprintf("2026-07-31T12:00:%02dZ", i%60),
	}
}

// bufferedPoints returns a copy of what the buffer currently holds for a
// drive, read under the buffer's own mutex.
func bufferedPoints(t *testing.T, rb *routeBuffer, driveID string) []RoutePointRecord {
	t.Helper()
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return append([]RoutePointRecord(nil), rb.buffers[driveID]...)
}

// hasBufferEntry reports whether the map still holds an entry for the
// drive. Distinct from "the entry is empty": a permanent-failure drop
// must FREE the map entry, not leave a zero-length slice behind.
func hasBufferEntry(t *testing.T, rb *routeBuffer, driveID string) bool {
	t.Helper()
	rb.mu.Lock()
	defer rb.mu.Unlock()
	_, ok := rb.buffers[driveID]
	return ok
}

// assertPoints compares an observed point sequence against the expected
// indices from routePoint, reporting the whole sequence on mismatch —
// ordering bugs are unreadable when only the first difference is shown.
func assertPoints(t *testing.T, got []RoutePointRecord, wantIdx []int, what string) {
	t.Helper()
	want := make([]RoutePointRecord, 0, len(wantIdx))
	for _, i := range wantIdx {
		want = append(want, routePoint(i))
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d points %v, want %d points %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: point %d = %v, want %v (full got=%v want=%v)", what, i, got[i], want[i], got, want)
		}
	}
}

// --- tests ------------------------------------------------------------

// TestRouteBufferAdd pins the flush trigger. `add`'s bool is the writer's
// only signal to flush synchronously on a GPS frame: returning true too
// early means a DB round-trip per sample, too late means the buffer
// (plaintext P1 coordinates) grows past the configured bound.
func TestRouteBufferAdd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		flushSize int
		adds      int
		wantFlush []bool // wantFlush[i] is add's return on the (i+1)-th add
	}{
		{
			name:      "below threshold never signals",
			flushSize: 5,
			adds:      4,
			wantFlush: []bool{false, false, false, false},
		},
		{
			name:      "signals exactly at threshold",
			flushSize: 3,
			adds:      3,
			wantFlush: []bool{false, false, true},
		},
		{
			name:      "keeps signalling at or above threshold",
			flushSize: 2,
			adds:      4,
			wantFlush: []bool{false, true, true, true},
		},
		{
			name:      "flush size of one signals immediately",
			flushSize: 1,
			adds:      2,
			wantFlush: []bool{true, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newFakeRoutePersister(t)
			rb := newRouteBufferForTest(t, p, tt.flushSize)

			wantIdx := make([]int, 0, tt.adds)
			for i := 0; i < tt.adds; i++ {
				if got := rb.add("drive-1", routePoint(i)); got != tt.wantFlush[i] {
					t.Fatalf("add #%d = %v, want %v", i+1, got, tt.wantFlush[i])
				}
				wantIdx = append(wantIdx, i)
			}

			// Accumulation is append-only and in arrival order: the
			// trail is a path, so a reordered buffer is a wrong map.
			assertPoints(t, bufferedPoints(t, rb, "drive-1"), wantIdx, "buffered")

			if n := p.callCount(); n != 0 {
				t.Fatalf("add must never persist on its own, got %d AppendRoutePoints calls", n)
			}
		})
	}
}

// TestRouteBufferDiscard covers the micro-drive path (MYR-160): the Drive
// row is about to be deleted, so any surviving buffered point would be
// appended to a dead drive — or resurrect the row.
func TestRouteBufferDiscard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		addTo         map[string]int // driveID → number of points
		discard       string
		flushDrive    string
		wantPersisted []int // point indices the persister must see, nil for none
	}{
		{
			name:       "discarded drive flushes nothing",
			addTo:      map[string]int{"drive-1": 3},
			discard:    "drive-1",
			flushDrive: "drive-1",
		},
		{
			name:          "discard is scoped to one drive",
			addTo:         map[string]int{"drive-1": 2, "drive-2": 2},
			discard:       "drive-1",
			flushDrive:    "drive-2",
			wantPersisted: []int{0, 1},
		},
		{
			name:       "discarding an unknown drive is a no-op",
			addTo:      nil,
			discard:    "drive-nope",
			flushDrive: "drive-nope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newFakeRoutePersister(t)
			rb := newRouteBufferForTest(t, p, 100)

			for driveID, n := range tt.addTo {
				for i := 0; i < n; i++ {
					rb.add(driveID, routePoint(i))
				}
			}

			rb.discard(tt.discard)
			if hasBufferEntry(t, rb, tt.discard) {
				t.Fatalf("discard(%q) left the buffer entry in place", tt.discard)
			}
			if tt.wantPersisted != nil {
				// The survivor's buffer is untouched by its neighbour's
				// discard — one dead micro-drive must not take another
				// car's in-flight trail with it.
				assertPoints(t, bufferedPoints(t, rb, tt.flushDrive), tt.wantPersisted, "buffer of "+tt.flushDrive+" after discard")
			}

			rb.flushDrive(context.Background(), tt.flushDrive)

			calls := p.snapshot()
			if tt.wantPersisted == nil {
				if len(calls) != 0 {
					t.Fatalf("expected no AppendRoutePoints call, got %d: %+v", len(calls), calls)
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("expected exactly 1 AppendRoutePoints call, got %d: %+v", len(calls), calls)
			}
			if calls[0].driveID != tt.flushDrive {
				t.Fatalf("flushed drive_id = %q, want %q", calls[0].driveID, tt.flushDrive)
			}
			assertPoints(t, calls[0].points, tt.wantPersisted, "persisted")
		})
	}
}

// TestRouteBufferFlushDriveSuccess pins the happy path: the persister
// receives every buffered point once, in arrival order, and the buffer is
// left empty so the next flush is free.
func TestRouteBufferFlushDriveSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		points int
	}{
		{name: "single point", points: 1},
		{name: "several points", points: 5},
		{name: "a full 1Hz batch", points: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newFakeRoutePersister(t)
			rb := newRouteBufferForTest(t, p, 100)

			wantIdx := make([]int, 0, tt.points)
			for i := 0; i < tt.points; i++ {
				rb.add("drive-1", routePoint(i))
				wantIdx = append(wantIdx, i)
			}

			rb.flushDrive(context.Background(), "drive-1")

			calls := p.snapshot()
			if len(calls) != 1 {
				t.Fatalf("expected 1 AppendRoutePoints call, got %d", len(calls))
			}
			if calls[0].driveID != "drive-1" {
				t.Fatalf("flushed drive_id = %q, want %q", calls[0].driveID, "drive-1")
			}
			assertPoints(t, calls[0].points, wantIdx, "persisted")

			if hasBufferEntry(t, rb, "drive-1") {
				t.Fatal("successful flush must free the buffer entry")
			}

			// A second flush has nothing left to write.
			rb.flushDrive(context.Background(), "drive-1")
			if n := p.callCount(); n != 1 {
				t.Fatalf("second flush of an emptied drive called the persister; total calls = %d", n)
			}
		})
	}
}

// TestRouteBufferFlushDriveTransientError pins the RETRY half of the
// MYR-433 split. A dropped connection or pool timeout leaves the points
// perfectly good, so they must survive to the next tick — losing a GPS
// trail because the pool blipped for a second is data loss the user sees
// as a hole in their route.
//
// The ordering assertion is the sharp edge: the re-buffer PREPENDS the
// failed batch ahead of anything added while the flush was in flight
// (`append(pts, rb.buffers[driveID]...)`), which is what keeps the trail
// chronological. Appending instead would interleave the retry behind
// newer samples and draw the route backwards.
func TestRouteBufferFlushDriveTransientError(t *testing.T) {
	t.Parallel()

	transient := errors.New("read tcp 10.0.0.1:5432: connection reset by peer")

	tests := []struct {
		name string
		// addDuringFlush is a point index added from inside the failing
		// AppendRoutePoints call, i.e. after flushDrive deleted the map
		// entry. -1 means "add nothing mid-flush".
		addDuringFlush int
		wantAfterFail  []int
		wantRetry      []int
	}{
		{
			name:           "failed batch is re-buffered intact",
			addDuringFlush: -1,
			wantAfterFail:  []int{0, 1, 2},
			wantRetry:      []int{0, 1, 2},
		},
		{
			name:           "failed batch is prepended ahead of points added meanwhile",
			addDuringFlush: 9,
			wantAfterFail:  []int{0, 1, 2, 9},
			wantRetry:      []int{0, 1, 2, 9},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Fail the first flush, succeed the second.
			p := newFakeRoutePersister(t, transient, nil)
			rb := newRouteBufferForTest(t, p, 100)

			if tt.addDuringFlush >= 0 {
				idx := tt.addDuringFlush
				p.onCall = func(call int) {
					if call == 0 {
						rb.add("drive-1", routePoint(idx))
					}
				}
			}

			for i := 0; i < 3; i++ {
				rb.add("drive-1", routePoint(i))
			}

			rb.flushDrive(context.Background(), "drive-1")

			calls := p.snapshot()
			if len(calls) != 1 {
				t.Fatalf("expected 1 AppendRoutePoints call, got %d", len(calls))
			}
			assertPoints(t, calls[0].points, []int{0, 1, 2}, "first (failing) flush")
			assertPoints(t, bufferedPoints(t, rb, "drive-1"), tt.wantAfterFail, "re-buffered after transient failure")

			// The retry actually happens and carries everything.
			rb.flushDrive(context.Background(), "drive-1")

			calls = p.snapshot()
			if len(calls) != 2 {
				t.Fatalf("expected a retry (2 calls), got %d", len(calls))
			}
			assertPoints(t, calls[1].points, tt.wantRetry, "retried flush")

			if hasBufferEntry(t, rb, "drive-1") {
				t.Fatal("successful retry must free the buffer entry")
			}
		})
	}
}

// TestRouteBufferFlushDrivePermanentError is the important one. It pins
// the MYR-433 fix: a flush that failed because the drive's stored trail
// will not decrypt must DROP the batch and free the entry, never
// re-buffer.
//
// Retrying such a failure is not merely useless, it is a hot loop with a
// leak attached. `add` returns true once a drive holds FlushSize points
// and the writer then flushes SYNCHRONOUSLY on the GPS frame, so at 1Hz a
// poisoned drive attempts a doomed Begin + SELECT FOR UPDATE + decrypt +
// Rollback on EVERY sample; meanwhile the re-buffer means nothing but
// drive.discarded ever frees the entry, so an unbounded in-memory buffer
// of plaintext P1 coordinates grows for as long as the car is moving —
// and the 10s ticker keeps retrying forever after the drive has ended.
//
// The wrapped-error rows matter too: AppendRoutePoints wraps with
// fmt.Errorf("...: %w", ...), so this classification must be errors.Is,
// not ==. An equality check would silently fall through to the transient
// branch in production while still passing a naive test.
func TestRouteBufferFlushDrivePermanentError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		// wantDropped: batch discarded and entry freed (permanent).
		// Otherwise the points must be re-buffered (transient).
		wantDropped bool
	}{
		{
			name:        "unreadable trail is dropped",
			err:         ErrRouteTrailUnreadable,
			wantDropped: true,
		},
		{
			name:        "wrapped unreadable trail is dropped",
			err:         fmt.Errorf("store.AppendRoutePoints(drive-1): %w", ErrRouteTrailUnreadable),
			wantDropped: true,
		},
		{
			name:        "doubly wrapped unreadable trail is dropped",
			err:         fmt.Errorf("writer: %w", fmt.Errorf("store.AppendRoutePoints: %w", ErrRouteTrailUnreadable)),
			wantDropped: true,
		},
		{
			name:        "missing encryptor is dropped",
			err:         ErrEncryptionRequired,
			wantDropped: true,
		},
		{
			name:        "wrapped missing encryptor is dropped",
			err:         fmt.Errorf("store.AppendRoutePoints(drive-1): %w", ErrEncryptionRequired),
			wantDropped: true,
		},
		{
			name:        "joined error containing a permanent cause is dropped",
			err:         errors.Join(errors.New("flush batch"), ErrRouteTrailUnreadable),
			wantDropped: true,
		},
		{
			// Control row: proves the drop is keyed on the sentinel and
			// not on "any error at all".
			name:        "generic database error is retried",
			err:         errors.New("timeout: pool exhausted"),
			wantDropped: false,
		},
		{
			// ErrDriveNotFound is deliberately transient: it is usually a
			// race with a micro-drive discard, and the retry is harmless.
			name:        "drive not found is retried",
			err:         fmt.Errorf("store.AppendRoutePoints(drive-1): %w", ErrDriveNotFound),
			wantDropped: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Only the first call fails; a re-buffered batch would
			// therefore succeed on the retry and show up as a 2nd call.
			p := newFakeRoutePersister(t, tt.err, nil)
			rb := newRouteBufferForTest(t, p, 100)

			for i := 0; i < 3; i++ {
				rb.add("drive-1", routePoint(i))
			}

			rb.flushDrive(context.Background(), "drive-1")

			if n := p.callCount(); n != 1 {
				t.Fatalf("expected 1 AppendRoutePoints call, got %d", n)
			}

			if !tt.wantDropped {
				assertPoints(t, bufferedPoints(t, rb, "drive-1"), []int{0, 1, 2}, "re-buffered after transient failure")
				rb.flushDrive(context.Background(), "drive-1")
				if n := p.callCount(); n != 2 {
					t.Fatalf("transient failure must be retried; call count = %d, want 2", n)
				}
				return
			}

			// The batch is gone AND the map entry is freed — a
			// zero-length slice left behind would still be a per-drive
			// leak across a fleet of poisoned drives.
			if got := bufferedPoints(t, rb, "drive-1"); len(got) != 0 {
				t.Fatalf("permanent failure re-buffered %d points (%v); the batch must be dropped", len(got), got)
			}
			if hasBufferEntry(t, rb, "drive-1") {
				t.Fatal("permanent failure left a buffer entry behind; it must be freed")
			}

			// The hot loop: a second flush must not touch the database.
			rb.flushDrive(context.Background(), "drive-1")
			if n := p.callCount(); n != 1 {
				t.Fatalf("permanent failure retried: call count = %d after a second flush, want 1", n)
			}

			// And the drive can still take new points without dragging
			// the dropped batch along.
			rb.add("drive-1", routePoint(7))
			rb.flushDrive(context.Background(), "drive-1")
			calls := p.snapshot()
			if len(calls) != 2 {
				t.Fatalf("expected the post-drop flush to be call #2, got %d calls", len(calls))
			}
			assertPoints(t, calls[1].points, []int{7}, "post-drop flush")
		})
	}
}

// TestRouteBufferFlushDriveNoPoints pins the cheap-exit contract: an
// unknown or already-drained drive must not open a transaction. The
// ticker calls flushAll every 10s and drive.ended flushes on the way out,
// so a persister call here would be a steady stream of empty writes.
func TestRouteBufferFlushDriveNoPoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prime   bool // buffer and drain a drive first
		flushID string
	}{
		{name: "never-seen drive", prime: false, flushID: "drive-unknown"},
		{name: "empty string drive id", prime: false, flushID: ""},
		{name: "already drained drive", prime: true, flushID: "drive-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newFakeRoutePersister(t)
			rb := newRouteBufferForTest(t, p, 100)

			want := 0
			if tt.prime {
				rb.add("drive-1", routePoint(0))
				rb.flushDrive(context.Background(), "drive-1")
				want = 1
			}

			rb.flushDrive(context.Background(), tt.flushID)

			if n := p.callCount(); n != want {
				t.Fatalf("AppendRoutePoints calls = %d, want %d", n, want)
			}
			if hasBufferEntry(t, rb, tt.flushID) {
				t.Fatalf("flushDrive(%q) created a buffer entry", tt.flushID)
			}
		})
	}
}

// TestRouteBufferFlushAll pins the drain used by the 10s ticker and by
// shutdown: every buffered drive is written, not just the busiest one. A
// partial drain would silently strand the trail of any car that stopped
// reporting mid-window.
func TestRouteBufferFlushAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		buffered map[string]int // driveID → points
	}{
		{name: "no drives buffered", buffered: nil},
		{name: "one drive", buffered: map[string]int{"drive-1": 3}},
		{
			name:     "several drives",
			buffered: map[string]int{"drive-1": 1, "drive-2": 4, "drive-3": 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newFakeRoutePersister(t)
			rb := newRouteBufferForTest(t, p, 100)

			for driveID, n := range tt.buffered {
				for i := 0; i < n; i++ {
					rb.add(driveID, routePoint(i))
				}
			}

			rb.flushAll(context.Background())

			calls := p.snapshot()
			if len(calls) != len(tt.buffered) {
				t.Fatalf("AppendRoutePoints calls = %d, want %d (one per buffered drive): %+v",
					len(calls), len(tt.buffered), calls)
			}

			seen := make(map[string][]RoutePointRecord, len(calls))
			for _, c := range calls {
				if _, dup := seen[c.driveID]; dup {
					t.Fatalf("drive %q was flushed twice", c.driveID)
				}
				seen[c.driveID] = c.points
			}
			for driveID, n := range tt.buffered {
				got, ok := seen[driveID]
				if !ok {
					t.Fatalf("drive %q was never flushed", driveID)
				}
				wantIdx := make([]int, 0, n)
				for i := 0; i < n; i++ {
					wantIdx = append(wantIdx, i)
				}
				assertPoints(t, got, wantIdx, "flushAll of "+driveID)
			}

			rb.mu.Lock()
			left := len(rb.buffers)
			rb.mu.Unlock()
			if left != 0 {
				t.Fatalf("flushAll left %d buffer entries behind", left)
			}
		})
	}
}

// TestRouteBufferConcurrentAddAndFlush is the race-detector smoke test.
// In production `add` runs on the telemetry goroutine while the ticker
// runs flushAll and drive.ended runs flushDrive, all against the same
// map — so the invariant is not just "no data race" but "no point is
// lost or duplicated when a flush interleaves with an add".
//
// Deterministic by construction: no sleeps, all goroutines joined via a
// WaitGroup, and the final flushAll happens after every producer has
// returned. Run with -race.
func TestRouteBufferConcurrentAddAndFlush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		drives            int
		producers         int // per drive
		pointsPerProducer int
		flushers          int
		flushRounds       int
	}{
		{
			name:              "single drive, many writers",
			drives:            1,
			producers:         8,
			pointsPerProducer: 50,
			flushers:          4,
			flushRounds:       50,
		},
		{
			name:              "many drives",
			drives:            6,
			producers:         3,
			pointsPerProducer: 40,
			flushers:          3,
			flushRounds:       50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newFakeRoutePersister(t)
			// FlushSize 10 so producers also flush synchronously on the
			// `add` signal, exactly like the writer does at 1Hz.
			rb := newRouteBufferForTest(t, p, 10)

			driveIDs := make([]string, 0, tt.drives)
			for d := 0; d < tt.drives; d++ {
				driveIDs = append(driveIDs, fmt.Sprintf("drive-%d", d))
			}

			ctx := context.Background()
			var producersWG, flushersWG sync.WaitGroup
			done := make(chan struct{})

			for _, driveID := range driveIDs {
				for w := 0; w < tt.producers; w++ {
					producersWG.Add(1)
					go func(driveID string) {
						defer producersWG.Done()
						for i := 0; i < tt.pointsPerProducer; i++ {
							if rb.add(driveID, routePoint(i)) {
								rb.flushDrive(ctx, driveID)
							}
						}
					}(driveID)
				}
			}

			for f := 0; f < tt.flushers; f++ {
				flushersWG.Add(1)
				go func() {
					defer flushersWG.Done()
					for r := 0; r < tt.flushRounds; r++ {
						select {
						case <-done:
							return
						default:
						}
						rb.flushAll(ctx)
						runtime.Gosched()
					}
				}()
			}

			producersWG.Wait()
			close(done)
			flushersWG.Wait()

			// Final drain: everything must now be in the persister.
			rb.flushAll(ctx)

			persisted := 0
			for _, c := range p.snapshot() {
				if len(c.points) == 0 {
					t.Fatal("flushed an empty batch; flushDrive must exit early when there is nothing to write")
				}
				persisted += len(c.points)
			}

			want := tt.drives * tt.producers * tt.pointsPerProducer
			if persisted != want {
				t.Fatalf("persisted %d points, want %d (lost or duplicated under concurrency)", persisted, want)
			}

			rb.mu.Lock()
			left := len(rb.buffers)
			rb.mu.Unlock()
			if left != 0 {
				t.Fatalf("final flushAll left %d buffer entries behind", left)
			}
		})
	}
}
