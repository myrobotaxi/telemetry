---
name: map_interaction_analysis
description: Root cause analysis of the VehicleMap snap-back bug and the Tesla-like follow/free mode design needed to fix it — identified 2026-03-21
type: project
---

Root cause of the map snap-back bug: `useRouteLayer` calls `fitBounds` inside a `useEffect` whose dependency array includes `vehiclePosition`. Every telemetry update that changes the vehicle's coordinates causes the effect to re-run and re-call `fitBounds`, overriding any zoom or pan the user has performed.

**Why:** The effect was originally written to render the route, but it bundled route rendering and initial view-fitting into a single effect. As a result, the view-fit logic fires on every vehicle position change, not just when the route is first loaded.

**How to apply:** When working on the fix (or reviewing PRs that touch `useRouteLayer`), the fix must separate route rendering (runs on route change) from initial bounds fitting (runs only once per new route). A "follow mode" state machine should control when `flyTo` or `fitBounds` is called, and user interaction events (`dragstart`, `zoomstart`, `pitchstart`) should break follow mode until the recenter button is pressed.

The proposed Tesla-like model:
- Follow mode (default): `flyTo` on vehicle position updates, zoom ~15-16, smooth 1s transition
- Free mode: entered on any user gesture (`dragstart`, `zoomstart`); `fitBounds` and auto-`flyTo` both suppressed
- Recenter button: calls `flyTo` back to vehicle, re-enters follow mode
- Route overview button (existing `FitRouteButton`): calls `fitBounds` once on demand; does NOT re-enter follow mode
- The `isOffCenter` check in `useMapRecenter` already detects drift but does not suppress auto-center — it must be replaced by the follow/free state machine

Key files involved:
- `src/components/map/hooks/use-route-layer.ts` line 88 — `fitBounds` call inside effect with `vehiclePosition` dep
- `src/components/map/hooks/use-route-layer.ts` line 89 — dep array: `[map, mapLoaded, showRoute, routeCoordinates, vehiclePosition]`
- `src/features/vehicles/components/HomeScreen.tsx` line 96-104 — passes `center={[vehicle.longitude, vehicle.latitude]}` on every render
- No user-interaction detection currently exists anywhere in the map hooks
- `useMapRecenter` is the right hook to extend into a follow/free state machine, but today it has no effect on auto-center

GitHub issue to file: in the `my-robo-taxi` repo. No existing issue covers this — the closest open issue is #75 (vehicle switching UX) which is unrelated.
