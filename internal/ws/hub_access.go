package ws

import (
	"log/slog"
)

// Mid-connection access revocation for SHARE grants (MYR-373, closing
// websocket-protocol.md §10 DV-09).
//
// The problem this file solves: Client.vehicleIDs and Client.vehicleRoles are
// frozen at handshake (handler.go authenticateClient). Every access decision
// after that — Hub.Broadcast's hasVehicle, BroadcastMasked's roleFor — reads
// that frozen snapshot. So an owner who suspends or revokes a viewer stops
// their REST access within the cache TTL but leaves an already-open socket
// streaming the car's live GPS until the socket happens to reconnect. With
// strangers as viewers, "revoke" has to mean revoked in seconds.
//
// WHY CLOSE THE CONNECTION RATHER THAN DROP THE ONE VEHICLE:
//
//  1. THE PROTOCOL ALREADY DOCUMENTS THIS EXACT CLOSE. §6.2 defines close
//     code 4002 "Permission Revoked — Vehicle ownership revoked while
//     connected (e.g., invite removed)", with the client guidance "surface to
//     UI; do not auto-retry the same vehicle". There is no per-vehicle
//     "removal" frame in the v1 protocol, and inventing one would be a WIRE
//     CHANGE that every deployed SDK would have to learn. Closing with 4002 is
//     server behavior only: zero wire change, and the SDKs already handle it
//     because the vehicle-deletion path (MYR-73) has emitted it since May.
//
//  2. RE-DERIVING IN PLACE WOULD PUT A WRITE ON THE HOTTEST READ PATH.
//     vehicleIDs is written once and thereafter read lock-free by every
//     broadcast; mutating it mid-connection means synchronizing the fan-out
//     for every vehicle on every frame to fix an owner action that happens a
//     handful of times a day.
//
//  3. RECONNECT RE-DERIVES EVERYTHING, CORRECTLY, IN ONE PLACE. The handshake
//     already refetches the access set AND the per-vehicle roles. A viewer who
//     lost one of three cars reconnects and comes back with the other two,
//     with roles resolved fresh — which also happens to fix the role-downgrade
//     half of DV-09 for free, because there is no stale snapshot left to
//     downgrade.
//
// The cost is that a grantee holding several shared cars is briefly
// disconnected from all of them when they lose one. That is the same trade the
// vehicle-deletion path already makes, it is invisible behind the SDK's
// reconnect, and it fails in the safe direction.

// RevokeUserAccess closes every session belonging to userID that is
// authorized for (or actively subscribed to) vehicleID, with close code 4002
// and reason "vehicle access revoked" — the same frame §6.2 already defines
// and the same one Hub.RemoveVehicle emits, so no client needs new handling.
//
// SCOPED TO ONE USER. Unlike RemoveVehicle, which closes everybody on a
// vehicle because the vehicle is gone, this closes only the person whose grant
// moved. The owner's own session, and every other viewer's, keeps streaming:
// a suspension is one grant changing, not the car going away.
//
// An empty vehicleID means "every session this user holds" and is the blunt
// fallback for a caller that could not determine which vehicle was lost. An
// empty userID is a no-op, NOT a wildcard: a malformed revocation must never
// be read as "close everyone".
//
// Safe to call for a user with no live sessions (the common case — most
// revocations target somebody who is not connected), and idempotent: closing
// an already-closed connection returns an error that is deliberately ignored.
//
// RETURNS AS SOON AS THE SESSIONS ARE CUT OFF, not when their TCP connections
// finish dying, and the split matters. A graceful WebSocket close writes the
// close frame and then WAITS for the peer to echo it — measured at the
// library's full five-second ceiling against a peer that never answers. Doing
// that inline would block the caller, which is a single per-subscription bus
// goroutine: one unresponsive viewer would stall every later revocation behind
// it, on a bus that drops the oldest event when a subscriber falls behind. So
// the flag is set synchronously (that is what actually stops the frames) and
// the close handshake is handed to a goroutine that lives at most as long as
// the library's own timeout.
func (h *Hub) RevokeUserAccess(userID, vehicleID, reason string) int {
	if userID == "" {
		return 0
	}

	affected := h.collectUserSessions(userID, vehicleID)
	for _, client := range affected {
		// FIRST, and synchronously: no further frame for this session,
		// whatever the close handshake does next.
		//
		// The flag is enforced in Client.enqueue, which is the ONLY writer to
		// the send channel — so it covers the broadcast paths AND the
		// snapshot-on-subscribe path, which reaches the client without
		// consulting hasVehicle at all. An earlier version of this comment
		// claimed hasVehicle was the choke point; it was not, and a revoked
		// session could still pull live GPS by sending `subscribe` during the
		// close window. sendSnapshot and handleSubscribeFrame check the flag
		// too, so the refusal happens before the database read rather than at
		// the channel.
		//
		// markRevoked also wakes writePump. That is load-bearing, not
		// housekeeping: once enqueue refuses everything, writePump can never
		// be woken by a message again, and it holds g.Wait() — and therefore
		// Unregister — behind it. Setting the flag alone leaks the session's
		// goroutines and its hub entry permanently.
		client.markRevoked()

		if client.conn != nil {
			conn := client.conn
			go func() {
				// The close FRAME is written before this blocks on the echo,
				// so the client sees 4002 immediately; only the teardown of
				// the TCP connection waits.
				_ = conn.Close(closeCodeVehicleAccessRevoked, vehicleAccessRevokedReason)
			}()
		}

		h.logger.Info("closing client: share access revoked",
			slog.String("user_id", client.userID),
			slog.String("vehicle_id", vehicleID),
			slog.String("reason", reason),
			slog.Int("close_code", closeCodeVehicleAccessRevoked),
		)
	}
	return len(affected)
}

// collectUserSessions snapshots this user's sessions that cover vehicleID (or
// all of them when vehicleID is empty) under the read lock, so the close
// handshakes happen outside h.mu.
func (h *Hub) collectUserSessions(userID, vehicleID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var affected []*Client
	for client := range h.clients {
		if client.userID != userID {
			continue
		}
		// Already torn down and just waiting for its readPump to notice.
		// Skipping keeps the returned count honest and avoids spawning a
		// second close goroutine for a session that is already going —
		// which matters because the 60s backstop sweep can land on top of a
		// nudge that fired moments earlier.
		if client.revoked.Load() {
			continue
		}
		if vehicleID != "" && !clientAuthorizedForVehicle(client, vehicleID) {
			continue
		}
		affected = append(affected, client)
	}
	return affected
}

// snapshotClients returns the currently registered clients. Taken under the
// read lock and returned as a slice so callers — the periodic revalidation
// backstop in access_revalidator.go — can do slow per-client work (a database
// round trip, a close handshake) without holding h.mu.
//
// The snapshot may go stale the instant it is returned: a client in it may
// already have disconnected. Every consumer must therefore tolerate operating
// on a dead client, which they do — Close on a closed conn is an ignored
// error, and Unregister is idempotent.
func (h *Hub) snapshotClients() []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]*Client, 0, len(h.clients))
	for client := range h.clients {
		out = append(out, client)
	}
	return out
}

// authorizedVehicles returns the union of the client's handshake-time access
// set and its active subscription set. The union rather than either one alone:
// a vehicle in the handshake set but not currently subscribed still starts
// flowing the moment the client re-subscribes, so for revocation purposes the
// client "holds" it — the same reasoning clientAuthorizedForVehicle uses.
func (c *Client) authorizedVehicles() []string {
	held := make(map[string]struct{}, len(c.vehicleIDs))
	for _, vid := range c.vehicleIDs {
		held[vid] = struct{}{}
	}

	c.subMu.RLock()
	for vid := range c.subscribed {
		held[vid] = struct{}{}
	}
	c.subMu.RUnlock()

	out := make([]string, 0, len(held))
	for vid := range held {
		out = append(out, vid)
	}
	return out
}
