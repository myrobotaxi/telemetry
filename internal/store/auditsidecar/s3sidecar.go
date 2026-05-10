package auditsidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"
)

const (
	// defaultQueueSize is the bounded channel capacity. At ~1 KB per
	// JSON entry, 10 000 entries ≈ 10 MB of in-process buffer — well
	// within acceptable RSS overhead.
	defaultQueueSize = 10_000

	// maxAttempts is how many times the worker retries a single PutObject
	// before giving up and incrementing the failure counter.
	maxAttempts = 3

	// baseRetryDelay is the initial back-off interval; doubled on each
	// subsequent attempt. Attempt 1 waits 0 s (immediate), attempt 2
	// waits 1 s, attempt 3 waits 2 s.
	baseRetryDelay = time.Second

	// drainTimeout is the maximum time Close() waits for the in-process
	// queue to drain before giving up and returning.
	drainTimeout = 30 * time.Second
)

// ObjectPutter is the narrow S3 interface the sidecar needs. Using an
// interface instead of *s3.Client makes unit testing trivial — inject a
// fake that records calls rather than hitting real AWS.
type ObjectPutter interface {
	PutObject(ctx context.Context, bucket, key string, body []byte) error
}

// S3Sidecar is the production Sidecar implementation. It wraps an
// ObjectPutter (backed by aws-sdk-go-v2/service/s3 in production) and a
// bounded in-process channel. The worker goroutine started by Start() drains
// the channel and writes each entry to S3 with exponential back-off.
//
// Construct via NewS3Sidecar; close via Close to drain the queue.
type S3Sidecar struct {
	bucket  string
	putter  ObjectPutter
	metrics Metrics
	logger  *slog.Logger
	queue   chan AuditEntry
	done    chan struct{}
}

// S3SidecarConfig holds constructor parameters.
type S3SidecarConfig struct {
	// Bucket is the S3 bucket name (AUDIT_SIDECAR_BUCKET).
	Bucket string
	// QueueSize overrides the default channel capacity. 0 → defaultQueueSize.
	QueueSize int
}

// NewS3Sidecar constructs an S3Sidecar and starts the background worker.
// Call Close(ctx) when the process is shutting down so the queue drains
// before the process exits.
func NewS3Sidecar(cfg S3SidecarConfig, putter ObjectPutter, m Metrics, logger *slog.Logger) *S3Sidecar {
	qSize := cfg.QueueSize
	if qSize <= 0 {
		qSize = defaultQueueSize
	}
	s := &S3Sidecar{
		bucket:  cfg.Bucket,
		putter:  putter,
		metrics: m,
		logger:  logger,
		queue:   make(chan AuditEntry, qSize),
		done:    make(chan struct{}),
	}
	go s.worker()
	return s
}

// Emit enqueues an AuditEntry for async S3 upload. It never blocks on I/O.
// Returns ErrQueueFull if the channel is at capacity; the caller must
// increment a metric and log but must not propagate this error to the DB
// caller.
func (s *S3Sidecar) Emit(entry AuditEntry) error {
	select {
	case s.queue <- entry:
		s.metrics.SetQueueDepth(len(s.queue))
		return nil
	default:
		s.metrics.IncFailure("enqueue_full")
		return ErrQueueFull
	}
}

// Close signals the worker to drain the remaining queue entries and waits for
// it to finish (up to drainTimeout). Call from the graceful-shutdown chain.
func (s *S3Sidecar) Close(ctx context.Context) error {
	close(s.queue) // signal worker to drain and exit
	select {
	case <-s.done:
		return nil
	case <-time.After(drainTimeout):
		return fmt.Errorf("auditsidecar: Close timed out after %s; %d entries may not have been written", drainTimeout, len(s.queue))
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker drains the queue, writing each entry to S3 with retries.
// Runs until the queue channel is closed (by Close) and drained.
func (s *S3Sidecar) worker() {
	defer close(s.done)
	for entry := range s.queue {
		s.metrics.SetQueueDepth(len(s.queue))
		s.writeWithRetry(entry)
	}
}

// writeWithRetry attempts PutObject up to maxAttempts times with exponential
// back-off. On permanent failure it increments the aws failure counter and
// logs a Warn.
func (s *S3Sidecar) writeWithRetry(entry AuditEntry) {
	key := objectKey(entry)
	body, err := marshalEntry(entry)
	if err != nil {
		s.logWarn("auditsidecar: failed to marshal entry",
			slog.String("audit_log_id", entry.ID),
			slog.String("error", err.Error()))
		s.metrics.IncFailure("other")
		return
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * baseRetryDelay
			time.Sleep(delay)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		putErr := s.putter.PutObject(ctx, s.bucket, key, body)
		cancel()
		if putErr == nil {
			s.metrics.IncWrite()
			return
		}
		s.logWarn("auditsidecar: PutObject attempt failed",
			slog.String("audit_log_id", entry.ID),
			slog.String("key", key),
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", maxAttempts),
			slog.String("error", putErr.Error()))
	}

	s.metrics.IncFailure("aws")
	s.logWarn("auditsidecar: permanently dropping entry after all retries",
		slog.String("audit_log_id", entry.ID),
		slog.String("key", key),
		slog.Int("max_attempts", maxAttempts))
}

// logWarn guards against a nil logger — S3Sidecar accepts a nil logger in
// unit tests where log output is not desired.
func (s *S3Sidecar) logWarn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

// objectKey returns the S3 object key for an AuditEntry.
// Schema: audit/v1/{yyyy}/{mm}/{dd}/{userId}/{timestamp_unix_nanos}-{auditLogId}.json
//
// This layout lets operators run:
//
//	aws s3 ls s3://BUCKET/audit/v1/2026/05/09/
//
// to scan a single day across all users, or:
//
//	aws s3 ls s3://BUCKET/audit/v1/2026/05/09/{userId}/
//
// to scope to a single user.
func objectKey(e AuditEntry) string {
	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	return fmt.Sprintf("audit/v1/%04d/%02d/%02d/%s/%d-%s.json",
		ts.Year(), int(ts.Month()), ts.Day(),
		e.UserID,
		ts.UnixNano(),
		e.ID,
	)
}

// sidecarPayload is the JSON structure written to S3. It mirrors AuditEntry
// with JSON-friendly names and explicit UTC timestamps.
type sidecarPayload struct {
	ID         string          `json:"id"`
	UserID     string          `json:"userId"`
	Timestamp  string          `json:"timestamp"`
	Action     string          `json:"action"`
	TargetType string          `json:"targetType"`
	TargetID   string          `json:"targetId"`
	Initiator  string          `json:"initiator"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  string          `json:"createdAt"`
}

func marshalEntry(e AuditEntry) ([]byte, error) {
	meta := e.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	p := sidecarPayload{
		ID:         e.ID,
		UserID:     e.UserID,
		Timestamp:  e.Timestamp.UTC().Format(time.RFC3339Nano),
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Initiator:  e.Initiator,
		Metadata:   meta,
		CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("auditsidecar: marshalEntry: %w", err)
	}
	return b, nil
}
