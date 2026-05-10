package auditsidecar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePutter is a test double for ObjectPutter. It records every PutObject
// call and can be configured to return errors for the first N invocations.
type fakePutter struct {
	mu        sync.Mutex
	calls     []putCall
	errCount  int // number of leading errors to inject
	callCount int // total calls so far
}

type putCall struct {
	bucket string
	key    string
	body   []byte
}

func (f *fakePutter) PutObject(_ context.Context, bucket, key string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	if f.callCount <= f.errCount {
		return errors.New("fake S3 error")
	}
	f.calls = append(f.calls, putCall{bucket: bucket, key: key, body: body})
	return nil
}

func (f *fakePutter) recordedCalls() []putCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]putCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakePutter) totalCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// testEntry returns a minimal AuditEntry for testing.
func testEntry(id string) AuditEntry {
	return AuditEntry{
		ID:         id,
		UserID:     "user-test",
		Timestamp:  time.Now().UTC(),
		Action:     "account_deleted",
		TargetType: "user",
		TargetID:   id,
		Initiator:  "system",
	}
}

// Test 1: happy path — Emit an entry, drain queue, assert PutObject fired.
func TestS3Sidecar_HappyPath(t *testing.T) {
	fp := &fakePutter{}
	m := NoopMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "test-bucket", QueueSize: 16}, fp, m, nil)

	e := testEntry("id-happy")
	if err := s.Emit(e); err != nil {
		t.Fatalf("Emit() error = %v; want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	calls := fp.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PutObject call; got %d", len(calls))
	}
	if calls[0].bucket != "test-bucket" {
		t.Errorf("PutObject bucket = %q; want %q", calls[0].bucket, "test-bucket")
	}
	if !contains(calls[0].key, "id-happy") {
		t.Errorf("PutObject key %q does not contain audit log id", calls[0].key)
	}
	if !contains(string(calls[0].body), `"id":"id-happy"`) {
		t.Errorf("PutObject body missing id field; got %s", calls[0].body)
	}
}

// Test 2: queue overflow — fill the queue beyond capacity, assert ErrQueueFull
// AND that the enqueue_full counter incremented (the metric is the actual
// operator-facing contract this PR ships via
// audit_sidecar_write_failures_total{reason="enqueue_full"}).
func TestS3Sidecar_QueueOverflow(t *testing.T) {
	// blockingPutter never returns so the queue fills up.
	var readyCh = make(chan struct{})
	bp := &blockingPutter{readyCh: readyCh}

	m := &countingMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "b", QueueSize: 2}, bp, m, nil)

	// Fill the queue (size=2) plus one more to block the worker.
	// Emit 3 entries — first two fill the channel, third overflows.
	// Worker is blocked, so it pulls one off; the channel refills.
	// We need to push enough to ensure at least one overflow.
	overflowed := false
	for i := 0; i < 20; i++ {
		err := s.Emit(testEntry(uniqueID(i)))
		if errors.Is(err, ErrQueueFull) {
			overflowed = true
			break
		}
	}
	// Unblock the worker so it can drain, then close.
	close(readyCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Close(ctx)

	if !overflowed {
		t.Error("expected at least one ErrQueueFull during overflow; got none")
	}
	m.mu.Lock()
	enqueueFull := m.failures["enqueue_full"]
	m.mu.Unlock()
	if enqueueFull == 0 {
		t.Error("expected at least one enqueue_full failure-counter increment on queue overflow; got 0")
	}
}

// blockingPutter blocks on PutObject until readyCh is closed.
type blockingPutter struct {
	readyCh chan struct{}
}

func (b *blockingPutter) PutObject(ctx context.Context, _, _ string, _ []byte) error {
	select {
	case <-b.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// countingMetrics records failure reason counts.
type countingMetrics struct {
	mu       sync.Mutex
	failures map[string]int
	writes   int
	depth    int
}

func (c *countingMetrics) IncWrite() {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
}
func (c *countingMetrics) IncFailure(reason string) {
	c.mu.Lock()
	if c.failures == nil {
		c.failures = map[string]int{}
	}
	c.failures[reason]++
	c.mu.Unlock()
}
func (c *countingMetrics) SetQueueDepth(n int) {
	c.mu.Lock()
	c.depth = n
	c.mu.Unlock()
}

// Test 3: worker retry — fake returns error twice then succeeds; assert 3
// total PutObject calls and writes counter increments once.
//
// Important: poll for the success BEFORE Close. The shutdown path
// intentionally aborts in-flight retry sleeps to keep drain bounded
// (otherwise a single bad entry could exceed drainTimeout) — so calling
// Close while the worker is mid-retry would short-circuit the retry
// loop. This test is verifying the retry mechanism itself, not the
// shutdown abort, so we wait for the work to complete first.
func TestS3Sidecar_WorkerRetry(t *testing.T) {
	fp := &fakePutter{errCount: 2} // first 2 calls return error; 3rd succeeds
	m := &countingMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "b", QueueSize: 16}, fp, m, nil)

	if err := s.Emit(testEntry("retry-id")); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	// Wait for the worker to finish all 3 attempts (2 errors + 1 success).
	// Retry delays are 0 + 1s + 2s = 3s, plus per-attempt PutObject latency.
	// Poll up to 10s.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && fp.totalCallCount() < 3 {
		time.Sleep(50 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if fp.totalCallCount() != 3 {
		t.Errorf("expected exactly 3 PutObject calls (2 errors + 1 success); got %d", fp.totalCallCount())
	}
	m.mu.Lock()
	writes := m.writes
	m.mu.Unlock()
	if writes != 1 {
		t.Errorf("expected 1 successful write metric; got %d", writes)
	}
}

// Test 4: permanent failure — fake always errors; assert failure counter
// increments and worker continues.
func TestS3Sidecar_PermanentFailure(t *testing.T) {
	fp := &fakePutter{errCount: 9999} // always fails
	m := &countingMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "b", QueueSize: 16}, fp, m, nil)

	if err := s.Emit(testEntry("perm-fail-id")); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	m.mu.Lock()
	putFails := m.failures["put"]
	writes := m.writes
	m.mu.Unlock()

	if putFails != 1 {
		t.Errorf("expected 1 aws failure; got %d", putFails)
	}
	if writes != 0 {
		t.Errorf("expected 0 writes on permanent failure; got %d", writes)
	}
}

// Test 5: shutdown drain — enqueue N entries, call Close, assert all N writes
// happened before Close returns.
func TestS3Sidecar_ShutdownDrain(t *testing.T) {
	const n = 20
	fp := &fakePutter{}
	m := NoopMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "b", QueueSize: n * 2}, fp, m, nil)

	for i := 0; i < n; i++ {
		if err := s.Emit(testEntry(uniqueID(i))); err != nil {
			t.Fatalf("Emit(%d) error = %v", i, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	calls := fp.recordedCalls()
	if len(calls) != n {
		t.Errorf("after Close, expected %d PutObject calls; got %d", n, len(calls))
	}
}

// uniqueID generates a unique string id for test entries.
func uniqueID(i int) string {
	return "id-" + itoa(i)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestS3Sidecar_CloseIdempotent verifies a second Close call does not panic
// and returns nil cleanly (regression test for the close-on-closed-channel
// bug fixed alongside the send-on-closed-channel race).
func TestS3Sidecar_CloseIdempotent(t *testing.T) {
	fp := &fakePutter{}
	m := &countingMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "b"}, fp, m, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Errorf("second Close should be a no-op; got %v", err)
	}
	// And a third for good measure (operators tend to chain defers).
	if err := s.Close(ctx); err != nil {
		t.Errorf("third Close should be a no-op; got %v", err)
	}
}

// TestS3Sidecar_EmitAfterCloseDoesNotPanic races concurrent Emit calls with
// Close. Without the closed-flag guard + recover this fans out into a
// send-on-closed-channel panic that crashes the whole process. With the
// guard, all post-close Emits return ErrSidecarClosed and the process stays
// up.
func TestS3Sidecar_EmitAfterCloseDoesNotPanic(t *testing.T) {
	fp := &fakePutter{}
	m := &countingMetrics{}
	s := NewS3Sidecar(S3SidecarConfig{Bucket: "b", QueueSize: 4}, fp, m, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Spawn N senders racing with a single Close.
	const senders = 50
	const emitsPerSender = 20
	var wg sync.WaitGroup
	wg.Add(senders)
	closedSeen := int64(0)
	for i := 0; i < senders; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < emitsPerSender; j++ {
				if err := s.Emit(testEntry(uniqueID(id*1000 + j))); errors.Is(err, ErrSidecarClosed) {
					atomic.AddInt64(&closedSeen, 1)
				}
			}
		}(i)
	}

	// Close while senders are mid-flight.
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()

	// We don't assert how MANY Emits saw ErrSidecarClosed — that depends on
	// scheduling. We're asserting two things implicitly: (a) the test did
	// not panic (would crash the whole binary), and (b) Emit's contract
	// holds under the race.
	t.Logf("post-close Emits that observed ErrSidecarClosed: %d / %d",
		closedSeen, senders*emitsPerSender)
}
