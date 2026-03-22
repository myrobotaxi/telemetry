#!/usr/bin/env bash
# generate-routes.sh — Fetches real road-following routes from Mapbox Directions API
# and saves them as JSON files in configs/routes/ for the simulator.
#
# Usage:
#   MAPBOX_TOKEN=pk.xxx ./scripts/generate-routes.sh
#
# Requirements: curl, python3 (for JSON formatting)

set -euo pipefail

MAPBOX_TOKEN="${MAPBOX_TOKEN:?Set MAPBOX_TOKEN environment variable}"
ROUTES_DIR="$(cd "$(dirname "$0")/.." && pwd)/configs/routes"
mkdir -p "$ROUTES_DIR"

fetch_route() {
    local name="$1"
    local display_name="$2"
    local origin_name="$3"
    local origin_coords="$4"  # lng,lat
    local dest_name="$5"
    local dest_coords="$6"    # lng,lat
    local waypoints="$7"      # optional intermediate waypoints "lng,lat;lng,lat"

    local coords_param="${origin_coords};${dest_coords}"
    if [[ -n "$waypoints" ]]; then
        coords_param="${origin_coords};${waypoints};${dest_coords}"
    fi

    local url="https://api.mapbox.com/directions/v5/mapbox/driving/${coords_param}?geometries=geojson&access_token=${MAPBOX_TOKEN}"

    echo "Fetching route: ${display_name}..."

    local origin_lng origin_lat dest_lng dest_lat
    origin_lng=$(echo "$origin_coords" | cut -d, -f1)
    origin_lat=$(echo "$origin_coords" | cut -d, -f2)
    dest_lng=$(echo "$dest_coords" | cut -d, -f1)
    dest_lat=$(echo "$dest_coords" | cut -d, -f2)

    curl -s "$url" | python3 -c "
import json, sys
data = json.load(sys.stdin)
if data.get('code') != 'Ok':
    print(f'Mapbox error: {data}', file=sys.stderr)
    sys.exit(1)
route = data['routes'][0]
coords = route['geometry']['coordinates']
dist_miles = route['distance'] / 1609.344
result = {
    'name': '${display_name}',
    'origin': {'name': '${origin_name}', 'lat': ${origin_lat}, 'lng': ${origin_lng}},
    'destination': {'name': '${dest_name}', 'lat': ${dest_lat}, 'lng': ${dest_lng}},
    'totalDistanceMiles': round(dist_miles, 1),
    'coordinates': coords
}
print(json.dumps(result, indent=2))
print(f'  -> {len(coords)} coordinates, {dist_miles:.1f} miles', file=sys.stderr)
" > "${ROUTES_DIR}/${name}.json"

    echo "  Saved to configs/routes/${name}.json"
}

# --- Highway Drive: Dallas to McKinney via US-75 ---
fetch_route \
    "highway-drive" \
    "Dallas to McKinney via US-75" \
    "Downtown Dallas" \
    "-96.7970,32.7767" \
    "McKinney Town Center" \
    "-96.6153,33.1972" \
    ""

# --- City Drive: Dallas loop through Deep Ellum and Uptown ---
fetch_route \
    "city-drive" \
    "Dallas City Loop: Downtown to Deep Ellum to Uptown" \
    "Downtown Dallas" \
    "-96.7970,32.7767" \
    "Uptown Dallas" \
    "-96.8010,32.8020" \
    "-96.7730,32.7840"

echo "Done. Generated routes:"
ls -la "$ROUTES_DIR"/*.json
