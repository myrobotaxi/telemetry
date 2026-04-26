# Swift SDK — Apple platform lifecycle contract

**Status:** Active — v1.
**Owner:** `sdk-architect` agent (with `sdk-swift` implementing).
**Anchored:** NFR-3.10, NFR-3.11, NFR-3.34, NFR-3.35, NFR-3.36, NFR-3.36a-d.
**Scope:** Swift SDK only. The TypeScript SDK has no platform-lifecycle contract — browsers and Node do not suspend SDK code mid-task.

---

## 1. Why this doc exists

`websocket-protocol.md` §7 and `state-machine.md` §5 specify the SDK's reconnect, heartbeat, and snapshot-resume behavior in transport-neutral terms. Apple platforms add a layer the JS runtime does not have: **the OS will suspend the process mid-task**. A `URLSessionWebSocketTask` that was healthy a millisecond ago can be silently frozen and later thawed without any close frame. Reconnection therefore has two distinct triggers on Apple platforms:

1. **Transport-level** — what `state-machine.md` §1 specifies (`WS_CLOSED`, `WS_ERROR`, backoff-timer fire).
2. **OS-driven** — scene-foreground notifications and background-task wake-ups, specific to UIKit / AppKit / SwiftUI / WatchKit.

This document specifies (a) the SDK API surface for receiving (2) from the consumer, and (b) the per-platform consumer-side wiring that translates Apple's OS notifications into SDK calls. The SDK itself is UI-framework-agnostic per NFR-3.35 — it does NOT import SwiftUI, UIKit, AppKit, WatchKit, or BackgroundTasks. Consumers, who already run inside a UI-framework context, do the observing and forward events to the SDK via async methods.

## 2. Platform → notification matrix (consumer-side reference)

This matrix lists the OS notifications consumers MUST observe and which SDK methods they map to. The SDK does not see these names — it only receives the corresponding `handleForegroundTransition()` / `handleBackgroundTransition()` / `performBackgroundSnapshotRefresh()` / `performBackgroundDriveRoutePrefetch(maxDrives:)` calls.

| Platform | UI framework | Foreground notification (→ `handleForegroundTransition`) | Background notification (→ `handleBackgroundTransition`) | Background-task source (→ `performBackgroundSnapshotRefresh` / `performBackgroundDriveRoutePrefetch`) |
|---|---|---|---|---|
| iOS 26+ | SwiftUI | `ScenePhase.active` | `ScenePhase.background` | `BGAppRefreshTask`, `BGProcessingTask` |
| iOS 26+ | UIKit | `UIScene.willEnterForegroundNotification` | `UIScene.didEnterBackgroundNotification` | `BGAppRefreshTask`, `BGProcessingTask` |
| iPadOS 26+ | SwiftUI | `ScenePhase.active` | `ScenePhase.background` | `BGAppRefreshTask`, `BGProcessingTask` |
| iPadOS 26+ | UIKit | `UIScene.willEnterForegroundNotification` | `UIScene.didEnterBackgroundNotification` | `BGAppRefreshTask`, `BGProcessingTask` |
| macOS 26+ | SwiftUI | `ScenePhase.active` | `ScenePhase.background` | n/a (foreground triggers + persistent connection) |
| macOS 26+ | AppKit | `NSApplication.didBecomeActiveNotification` | `NSApplication.didResignActiveNotification` | n/a |
| watchOS 26+ | SwiftUI | `ScenePhase.active` | `ScenePhase.background` | `WKApplicationRefreshBackgroundTask` (no `BGAppRefreshTask` on watchOS) |
| watchOS 26+ | WatchKit | `WKApplicationDidBecomeActiveNotification` | `WKApplicationWillResignActiveNotification` | `WKApplicationRefreshBackgroundTask` |
| visionOS 26+ | SwiftUI | `ScenePhase.active` (window scenes and `ImmersiveSpace`) | `ScenePhase.background` | `BGAppRefreshTask` |
| visionOS 26+ | UIKit | `UIScene.willEnterForegroundNotification` | `UIScene.didEnterBackgroundNotification` | `BGAppRefreshTask` |

`ScenePhase.inactive` (and the equivalent `WKApplicationWillResignActiveNotification`) is intentionally NOT mapped to an SDK method — the SDK does not change connection state on transient inactive transitions (§3.4).

## 3. Consumer-driven lifecycle integration

The Swift SDK is UI-framework-agnostic per NFR-3.35 — it MUST NOT `import SwiftUI`, `import UIKit`, `import AppKit`, `import WatchKit`, or `import BackgroundTasks`. The consumer's app, which already runs inside one of those frameworks, observes scene transitions and forwards them to the SDK via a small lifecycle API. This keeps the SDK linkable from headless contexts (Swift on the server, command-line tools, test rigs) and from any UI-framework choice.

### 3.1 SDK lifecycle API

The SDK's public `MyRoboTaxiClient` actor exposes two foreground-lifecycle methods. Both are `async`, idempotent, and `Sendable`-safe.

```swift
public actor MyRoboTaxiClient {
    /// Call when the app or a scene is about to enter the foreground.
    /// Triggers an immediate reconnect attempt with retry counter reset (NFR-3.36a).
    /// Idempotent: rapid back-to-back calls collapse to a single reconnect attempt.
    nonisolated public func handleForegroundTransition() async

    /// Call when the app or a scene has entered the background.
    /// On watchOS, closes the WebSocket proactively (NFR-3.36c).
    /// On other platforms, no-op — the OS will suspend the socket; the §7.4.1 watchdog covers staleness.
    /// Idempotent.
    nonisolated public func handleBackgroundTransition() async
}
```

These methods are declared `nonisolated` so consumer call sites (most of which are `@MainActor`-isolated SwiftUI / UIKit / AppKit / WatchKit observers) can invoke them without the call site itself needing to enter the SDK actor's isolation. The `async` body internally hops into actor isolation to mutate state — the call site sees a clean `Task { await sdk.handleForegroundTransition() }` with no priority-inheritance surprises.

The SDK additionally exposes two background-task entry points consumers invoke from inside their registered task handlers (`BGTaskScheduler` on iOS / iPadOS / macOS / visionOS; `WKApplicationDelegate.handle(_:)` on watchOS):

```swift
extension MyRoboTaxiClient {
    /// Run from a background-task handler. Fetches the REST snapshot for every owned vehicle.
    /// Throws `CancellationError` when the consumer's expiration handler cancels the parent Task.
    /// No-op when `connectionState == .connected` (foreground session has fresher data).
    public func performBackgroundSnapshotRefresh() async throws

    /// Run from a background-task handler when on WiFi. Prefetches the top-N drive routes per rest-api.md §7.3.
    /// Honors `URLSessionConfiguration.allowsExpensiveNetworkAccess = false`.
    /// Throws `CancellationError` on parent-task cancellation.
    public func performBackgroundDriveRoutePrefetch(maxDrives: Int = 3) async throws
}
```

The SDK does NOT register or schedule background tasks. Registration (Info.plist `BGTaskSchedulerPermittedIdentifiers`, `BGTaskScheduler.shared.register`, `WKApplicationDelegate.handle(_:)`) is consumer responsibility.

### 3.2 Consumer wiring — examples

#### SwiftUI (iOS / iPadOS / macOS / watchOS / visionOS)

```swift
@main
struct MyApp: App {
    @Environment(\.scenePhase) private var scenePhase
    let sdk = MyRoboTaxiClient(/* ... */)

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environment(sdk)
        }
        .onChange(of: scenePhase) { _, newPhase in
            switch newPhase {
            case .active:
                Task { await sdk.handleForegroundTransition() }
            case .background:
                Task { await sdk.handleBackgroundTransition() }
            case .inactive:
                break // no-op — see §3.4
            @unknown default:
                break
            }
        }
    }
}
```

#### UIKit (iOS / iPadOS)

The SDK does NOT expose a singleton — see §9.2. Consumers construct `MyRoboTaxiClient` once at app launch (typically in their `AppDelegate`) and hold the instance themselves. The example below assumes the consumer has resolved that instance into a property `sdk: MyRoboTaxiClient` on the scene delegate.

The closure passed to `NotificationCenter.addObserver(forName:object:queue:using:)` is `@Sendable` under Swift 6. Capturing `[weak self]` would require `self` (a `UIResponder` subclass) to be `Sendable`, which it is not. The idiomatic fix is `[weak sdk = self.sdk]` — the actor IS `Sendable`, so this compiles cleanly and avoids the implicit retain-self trap.

```swift
final class SceneDelegate: UIResponder, UIWindowSceneDelegate {
    /// Consumer-owned. Resolved from the app's dependency container at scene creation time.
    let sdk: MyRoboTaxiClient

    init(sdk: MyRoboTaxiClient) {
        self.sdk = sdk
        super.init()
    }

    func scene(_ scene: UIScene, willConnectTo: UISceneSession, options: UIScene.ConnectionOptions) {
        let center = NotificationCenter.default
        center.addObserver(forName: UIScene.willEnterForegroundNotification, object: scene, queue: .main) { [weak sdk = self.sdk] _ in
            Task { await sdk?.handleForegroundTransition() }
        }
        center.addObserver(forName: UIScene.didEnterBackgroundNotification, object: scene, queue: .main) { [weak sdk = self.sdk] _ in
            Task { await sdk?.handleBackgroundTransition() }
        }
    }
}
```

#### AppKit (macOS)

```swift
final class AppDelegate: NSObject, NSApplicationDelegate {
    /// Consumer-owned. Constructed once when the app delegate initializes.
    let sdk: MyRoboTaxiClient

    override init() {
        self.sdk = MyRoboTaxiClient(/* config */)
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let center = NotificationCenter.default
        center.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak sdk = self.sdk] _ in
            Task { await sdk?.handleForegroundTransition() }
        }
        center.addObserver(forName: NSApplication.didResignActiveNotification, object: nil, queue: .main) { [weak sdk = self.sdk] _ in
            Task { await sdk?.handleBackgroundTransition() }
        }
    }
}
```

#### WatchKit (watchOS, classic)

```swift
final class ExtensionDelegate: NSObject, WKApplicationDelegate {
    /// Consumer-owned. Constructed by the extension delegate at app launch.
    let sdk: MyRoboTaxiClient

    override init() {
        self.sdk = MyRoboTaxiClient(/* config */)
        super.init()
    }

    func applicationDidBecomeActive() {
        Task { [sdk] in await sdk.handleForegroundTransition() }
    }

    func applicationWillResignActive() {
        Task { [sdk] in await sdk.handleBackgroundTransition() }
    }
}
```

### 3.3 SDK behavior on each call

#### `handleForegroundTransition()`

1. Cancel any currently-pending §1.4 backoff timer (`state-machine.md`).
2. Reset the retry counter to 0 (NFR-3.36a).
3. If `connectionState == .disconnected`, immediately fire `WS_OPEN_REQUESTED` (state-machine.md §1.3 C-2): transition `disconnected -> connecting` and run the §5.1 reconnect sequence.
4. If `connectionState == .connected`, do nothing — the watchdog (§7.4.1 of `websocket-protocol.md`) will catch any stale socket within at most 30 s, and the next watchdog-driven `WS_CLOSED` will re-enter the same flow with the retry counter still 0.
5. If `connectionState == .connecting`, do nothing — let the in-flight attempt complete or time out per the existing pre-`auth_ok` 6 s bound (websocket-protocol.md §2.3 rule 4).
6. Idempotent: repeated calls within a short window collapse to a single reconnect attempt (state-machine.md §5.2 rule 5).

#### `handleBackgroundTransition()`

1. **iOS / iPadOS / visionOS / macOS:** continue holding the WebSocket open if the OS permits. `URLSessionWebSocketTask` will be silently suspended by the OS within seconds of background entry on iOS-class platforms; that is expected and correct. The SDK does NOT proactively close on background.
2. **watchOS only** (`#if os(watchOS)` inside the SDK): proactively close the WebSocket per NFR-3.36c. watchOS extended-runtime sessions are scarce and must be released. Transition `connected -> disconnected` with reason `app_backgrounded`. On the next `handleForegroundTransition()` call, run §3.3 above; rehydration follows NFR-3.11 (REST snapshot before live stream).
3. Idempotent.

The watchOS branch is selected at compile time inside the SDK via `#if os(watchOS)` — consumers do NOT pass a platform marker. This keeps the public API uniform across Apple platforms while preserving the watchOS-specific behavior NFR-3.36c demands.

### 3.4 No SDK API for `inactive` / `willResignActive`

The transient `inactive` state on iOS (system alerts, control center pull-down, watch crown twist, command-center summon) is intentionally NOT exposed as an SDK lifecycle method. The SDK does not change connection state on this transition. Consumers MUST NOT forward `inactive` notifications to the SDK; doing so would cause unnecessary reconnect churn.

## 4. URLSessionConfiguration contract

The Swift SDK MUST construct its `URLSession` for both REST and WebSocket transports with the following configuration:

| Property | Value | Rationale |
|---|---|---|
| `waitsForConnectivity` | `true` | Defer connection attempts when offline rather than failing fast — the OS will surface connectivity changes back to the SDK and the §5.1 sequence runs once the network is available |
| `allowsExpensiveNetworkAccess` | `false` for REST prefetch (rest-api.md §7.3); `true` for the primary WebSocket and on-demand REST | Honors Low Data Mode and metered cellular for opportunistic prefetches; primary live transport is exempt because the user is actively engaged |
| `allowsConstrainedNetworkAccess` | same as `allowsExpensiveNetworkAccess` | Same Low Data Mode semantics |
| `multipathServiceType` | `.handover` for the WebSocket; `.none` for REST | Allows seamless WiFi↔cellular handover for the live stream without dropping the socket; REST requests don't benefit |
| `timeoutIntervalForRequest` | 30 s for REST; **not applicable** to WebSocket | REST timeout is long enough for snapshot fetches over slow networks; WebSocket liveness is governed by the §7.4.1 watchdog instead |
| `timeoutIntervalForResource` | 60 s for REST | Hard cap on a single REST call (e.g., a slow drive-route response) |

The SDK MUST NOT use `URLSessionConfiguration.background(withIdentifier:)` for the WebSocket. Background URL sessions don't support WebSocket tasks; the consumer-registered `BGAppRefreshTask` / `WKApplicationRefreshBackgroundTask` flow (§5) is the only supported path for background snapshot refresh.

## 5. Consumer-registered background tasks

Background-task scheduling is **consumer responsibility**. The consumer's app registers identifiers (`Info.plist BGTaskSchedulerPermittedIdentifiers` on iOS-class platforms) and writes the handler closure that calls into the SDK's `performBackgroundSnapshotRefresh()` / `performBackgroundDriveRoutePrefetch(maxDrives:)` methods.

This split keeps the SDK free of `BackgroundTasks` framework imports (which don't exist on watchOS) and lets consumers tune cadence, identifiers, and registration policy without changing the SDK.

### 5.1 BGAppRefreshTask — snapshot refresh (iOS / iPadOS / macOS / visionOS)

Identifier: consumer-chosen (e.g., `<bundle-id>.myrobotaxi.snapshotRefresh`). Default cadence: 30 minutes (consumer-tunable). The OS gives the handler roughly 30 seconds to complete.

```swift
import BackgroundTasks

// `sdk` here is the consumer-owned MyRoboTaxiClient instance, in scope wherever
// the consumer registers their BGTaskScheduler handlers (typically App init or
// AppDelegate.application(_:didFinishLaunchingWithOptions:)).
BGTaskScheduler.shared.register(forTaskWithIdentifier: "com.example.myrobotaxi.snapshotRefresh", using: nil) { [sdk] task in
    guard let task = task as? BGAppRefreshTask else { return }

    // Schedule the next refresh first so cancellation doesn't drop the cadence.
    let next = BGAppRefreshTaskRequest(identifier: task.identifier)
    next.earliestBeginDate = Date(timeIntervalSinceNow: 30 * 60)
    try? BGTaskScheduler.shared.submit(next)

    let work = Task {
        do {
            try await sdk.performBackgroundSnapshotRefresh()
            task.setTaskCompleted(success: true)
        } catch {
            task.setTaskCompleted(success: false)
        }
    }
    task.expirationHandler = { work.cancel() }
}
```

Strong capture of `sdk` (an actor, hence `Sendable`) is correct here: BGTaskScheduler handlers are one-shot, run independently of any view-controller lifecycle, and MUST complete or fail explicitly. A weak capture would race against app teardown and silently no-op.

The SDK's `performBackgroundSnapshotRefresh()` MUST:

1. Be safe to call concurrently with foreground SDK activity — actor isolation handles this.
2. Fetch the REST `GET /vehicles/{id}/snapshot` for every vehicle in the cached ownership set. No WebSocket activity.
3. Apply the snapshot to internal `@Observable` state.
4. Observe `Task.checkCancellation()` between vehicle fetches so the consumer's `expirationHandler` (which calls `Task.cancel()`) terminates the work promptly.
5. Be a no-op when `connectionState == .connected`: a long-lived foreground session has fresher data than any snapshot fetch could provide.

### 5.2 BGProcessingTask — drive-route prefetch (iOS / iPadOS / macOS / visionOS)

Identifier: consumer-chosen (e.g., `<bundle-id>.myrobotaxi.driveRoutePrefetch`). Used for warming the top-N drive routes (rest-api.md §7.3) in the background, only on WiFi. The consumer sets `task.requiresNetworkConnectivity = true` and `task.requiresExternalPower` per their own policy.

```swift
BGTaskScheduler.shared.register(forTaskWithIdentifier: "com.example.myrobotaxi.driveRoutePrefetch", using: nil) { [sdk] task in
    guard let task = task as? BGProcessingTask else { return }
    let work = Task {
        do {
            try await sdk.performBackgroundDriveRoutePrefetch(maxDrives: 3)
            task.setTaskCompleted(success: true)
        } catch {
            task.setTaskCompleted(success: false)
        }
    }
    task.expirationHandler = { work.cancel() }
}
```

The SDK's `performBackgroundDriveRoutePrefetch(maxDrives:)` MUST:

1. Honor `URLSessionConfiguration.allowsExpensiveNetworkAccess = false` so cellular requests are deferred (NFR-3.36b, rest-api.md §7.3).
2. Cap at the consumer-supplied `maxDrives` (default 3).
3. Observe `Task.checkCancellation()` between drive fetches.

### 5.3 watchOS — WKApplicationRefreshBackgroundTask

watchOS does not have `BackgroundTasks`. The consumer's `WKApplicationDelegate.handle(_:)` method dispatches `WKApplicationRefreshBackgroundTask` instances and calls the same SDK method:

```swift
final class ExtensionDelegate: NSObject, WKApplicationDelegate {
    /// Consumer-owned. Same instance shown in §3.2 WatchKit example.
    let sdk: MyRoboTaxiClient

    override init() {
        self.sdk = MyRoboTaxiClient(/* config */)
        super.init()
    }

    func handle(_ backgroundTasks: Set<WKRefreshBackgroundTask>) {
        for task in backgroundTasks {
            if let refresh = task as? WKApplicationRefreshBackgroundTask {
                Task { [sdk] in
                    try? await sdk.performBackgroundSnapshotRefresh()
                    refresh.setTaskCompletedWithSnapshot(false)
                }
            } else {
                task.setTaskCompletedWithSnapshot(false)
            }
        }
    }
}
```

watchOS does NOT provide an explicit expiration handler in the same shape as iOS; the OS terminates the task. The SDK MUST tolerate abrupt termination — `performBackgroundSnapshotRefresh()` MUST leave the SDK's internal state consistent if cancelled mid-fetch.

Drive-route prefetch is NOT supported on watchOS — bandwidth is too constrained (rest-api.md §7.3, NFR-3.36c).

## 6. watchOS-specific behavior

watchOS has the strictest lifecycle constraints in the Apple platform family. The Swift SDK MUST treat watchOS as the worst-case lifecycle target — anything that works correctly on watchOS works correctly on every other Apple platform.

| Constraint | Implication |
|---|---|
| Apps may be foregrounded for as little as **5 seconds** before backgrounding | Cold launches must reach a renderable state without waiting on a fresh WebSocket frame; first paint reads from REST snapshot (NFR-3.11) |
| Extended-runtime sessions terminate without warning | Treat session-end as `disconnected` (NOT `error`); rehydrate from snapshot on resume via the next `handleForegroundTransition()` call (NFR-3.36c) |
| Background WebSocket connections aren't supported on watchOS the way they are on iOS | Proactive close on `handleBackgroundTransition()`; rely on `WKApplicationRefreshBackgroundTask` (§5.3) for any scheduled work |
| Bandwidth is precious | Honor `URLSessionConfiguration.allowsExpensiveNetworkAccess = false` aggressively; never opportunistically prefetch on watchOS |

The SDK MUST NOT block first paint on the WebSocket. The watch UI renders from the REST snapshot; the WebSocket fills in live updates as they arrive.

## 7. visionOS-specific behavior

visionOS adds two scene types beyond standard iOS: `WindowGroup` (2D windows) and `ImmersiveSpace` (mixed/full immersion). Both are governed by `ScenePhase` from the consumer's perspective and map identically into `handleForegroundTransition()` / `handleBackgroundTransition()` from the SDK's perspective — no visionOS-specific SDK code path.

Two visionOS-specific notes for the consumer wiring:

1. **Immersive scene transitions** can be fast (`ImmersiveSpace.dismiss` returns within milliseconds). The SDK's foreground handler is idempotent (§3.3 rule 6), but the consumer's `.onChange(of: scenePhase)` may fire many times in rapid succession during immersion changes. That's fine — repeated calls collapse.
2. **Spatial audio / persistent windows** may keep the app in the foreground longer than a typical iOS session. The §7.4.1 liveness watchdog still applies — long foreground durations are not exempt.

## 8. macOS-specific behavior

macOS apps typically remain in the foreground for hours. The SDK's behavior on macOS is dominated by the §3 lifecycle wiring plus the §7.4.1 liveness watchdog. There is no `BGAppRefreshTask` analog on macOS — the SDK relies on the long-lived foreground connection.

The one macOS-specific consideration: when the user puts the Mac to sleep, the WebSocket socket is preserved by the OS but data flow halts. On wake, the consumer's `NSApplication.didBecomeActiveNotification` (or SwiftUI `.onChange(of: scenePhase) { case .active: ... }`) observer fires and calls `handleForegroundTransition()`. By that time the §7.4.1 watchdog has already detected silent staleness and transitioned `connected -> disconnected`; the SDK runs the standard reconnect sequence.

## 9. Anti-patterns

The split below mirrors the SDK / consumer responsibility boundary. SDK anti-patterns are enforced by `contract-guard` (Rule CG-SWIFT-1); consumer anti-patterns are documented as wiring guidance.

### 9.1 SDK-internal anti-patterns (Rule CG-SWIFT-1)

The Swift SDK module MUST NOT:

- **Import any UI framework.** `import SwiftUI`, `import UIKit`, `import AppKit`, `import WatchKit`, and `import BackgroundTasks` are all forbidden in the SDK target. Lifecycle and background-task wiring is consumer responsibility (§3, §5); the SDK exposes only async methods. This rule is what NFR-3.35 enforces and what makes the SDK linkable from headless Swift contexts.
- **Use `Timer` or `DispatchSourceTimer` for staleness detection.** Freshness is event-driven (NFR-3.7); the §7.4.1 watchdog is the only liveness mechanism.
- **Resume an in-flight `URLSessionWebSocketTask` from a background URL session** (`URLSessionConfiguration.background(withIdentifier:)`). Background URL sessions don't support WebSocket tasks.
- **Schedule reconnects from a `Task.detached`** outside the SDK actor's isolation. All reconnect / state-transition work MUST run inside the SDK actor.
- **Expose `Combine.Publisher`, `@Published`, or `ObservableObject` on the public API.** Use `@Observable` (Swift 5.9+ Observation framework) plus `AsyncStream` per NFR-3.34.

### 9.2 Consumer wiring anti-patterns (informational guidance)

Consumers SHOULD avoid:

- **Holding strong references to `BGTask` instances beyond the handler closure.** Retain cycles will cause termination assertions.
- **Calling `task.setTaskCompleted(success:)` from a `Task.detached`** outside the registered handler closure. The closure must remain on the foreground OS-managed dispatch.
- **Forwarding `ScenePhase.inactive` to the SDK.** `inactive` is transient (§3.4); forwarding it causes reconnect churn.
- **Using `Combine` or `@Published` to observe `ScenePhase` in their app.** Use SwiftUI's environment value (`@Environment(\.scenePhase)`) or `NotificationCenter.default.notifications(named:)` (an `AsyncSequence`).
- **Capturing `[weak self]` in `@Sendable` notification observer closures** when `self` is a `UIResponder`/`NSObject` subclass (i.e., not `Sendable`). Capture `[weak sdk = self.sdk]` instead — the actor is `Sendable` and weak capture lets the observer no-op if the SDK has been torn down.
- **Exposing the SDK as a singleton** in your own app (e.g., `MyRoboTaxiClient.shared`). The SDK does not ship a singleton, and consumers SHOULD NOT add one — singletons block multi-tenant test rigs and prevent dependency injection.

## 10. Cross-references

- [`state-machine.md`](state-machine.md) §1 — connection state machine, §5 — transport-level reconnect, §5.3 — consumer-driven foreground reconnect.
- [`websocket-protocol.md`](websocket-protocol.md) §2.3 — pre-`auth_ok` 6 s timer; §7 — heartbeat + reconnect; §7.5 — Apple suspend/resume binding.
- [`rest-api.md`](rest-api.md) §7.3 — drive-route lazy-fetch, Low Data Mode rules.
- [`docs/architecture/requirements.md`](../architecture/requirements.md) §3.12 — platform support, NFR-3.34/35/36/36a-d.

## 11. Change log

| Date | Change | Reviewer |
|---|---|---|
| 2026-04-25 | Initial authoring as part of the RN-removal audit. Establishes Apple platform lifecycle contract previously implicit/missing. Introduces NFR-3.36a-d. | sdk-architect agent |
| 2026-04-25 | Reframed lifecycle integration as **consumer-driven**: SDK exposes `handleForegroundTransition()` / `handleBackgroundTransition()` / `performBackgroundSnapshotRefresh()` / `performBackgroundDriveRoutePrefetch(maxDrives:)` async methods; consumers observe Apple OS notifications and forward to those methods. Resolves contradiction with NFR-3.35 (no UI-framework imports). Anti-patterns split into SDK-internal (Rule CG-SWIFT-1) vs consumer-wiring (informational). | sdk-architect agent |
| 2026-04-26 | Post-review fixes (sdk-swift agent): (1) lifecycle methods declared `nonisolated` so `@MainActor` call sites don't hop through actor isolation; (2) renamed from `application*` (UIKit-coded) to `handle*Transition` (framework-neutral, mirrors CKSyncEngine precedent); (3) dropped `MyRoboTaxiClient.shared` from all wiring examples — SDK exposes no singleton; (4) wiring examples now capture `[weak sdk = self.sdk]` instead of `[weak self]` (UIResponder/NSObject are not `Sendable`); (5) BGTaskScheduler examples use strong `[sdk]` capture (one-shot handlers); (6) deleted obsolete "outside `MainActor`" anti-pattern bullet (obviated by `nonisolated`). | sdk-swift + sdk-architect agents |
