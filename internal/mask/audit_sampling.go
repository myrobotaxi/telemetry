package mask

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// Sampling rate for the mask audit log. Per docs/contracts/rest-api.md
// §5.3, every masked response (REST or WS) MUST be audit-logged at a
// 1% deterministic rate computed by hash modulo 100.
const auditSampleModulus uint64 = 100

// ShouldAuditREST returns true when this REST response should be
// emitted to the audit log. The decision is deterministic given the
// inputs — replaying the same userID + requestID + resourceID will
// always return the same boolean. Per rest-api.md §5.3, the inputs are
// joined with a separator before hashing to avoid collisions across
// distinct triples that share a concatenated form.
func ShouldAuditREST(userID, requestID, resourceID string) bool {
	h := fnv.New64a()
	writeField(h, userID)
	writeField(h, requestID)
	writeField(h, resourceID)
	return h.Sum64()%auditSampleModulus == 0
}

// ShouldAuditWS returns true when this WebSocket frame should be
// audit-logged. Per rest-api.md §5.3, the WS audit emit is per
// (vehicleID, role, frame) at the hub layer (NOT per client) — the
// hash inputs reflect that scope.
//
// frameSeq SHOULD be the envelope sequence number once DV-02 lands.
// Until then, callers can pass a per-vehicle monotonic counter — the
// determinism only requires that the counter is reproducible during
// replay.
func ShouldAuditWS(vehicleID string, role auth.Role, frameSeq uint64) bool {
	h := fnv.New64a()
	writeField(h, vehicleID)
	writeField(h, string(role))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], frameSeq)
	_, _ = h.Write(buf[:])
	return h.Sum64()%auditSampleModulus == 0
}

// writeField writes a length-prefixed string into the hash. The length
// prefix prevents ambiguity between (e.g.) {"abc", "def"} and
// {"ab", "cdef"} which would otherwise hash to the same byte sequence
// when concatenated. `hash.Hash64` already implements `io.Writer`, so
// the hasher is passed in directly without an inline anonymous
// interface (PR #195 review suggestion #2).
func writeField(h io.Writer, s string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(s))))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(s))
}
