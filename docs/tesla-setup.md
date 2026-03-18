# Tesla Fleet Telemetry Setup Guide

Complete step-by-step guide for configuring Tesla Fleet Telemetry with the MyRoboTaxi telemetry server.

## Overview

Tesla Fleet Telemetry allows third-party applications to receive real-time vehicle data (speed, location, charge, etc.) via a push-based protocol. The vehicle establishes an mTLS WebSocket connection to your server and streams protobuf-encoded telemetry at configured intervals.

```
Tesla Vehicle ──mTLS/WSS (port 443)──> MyRoboTaxi Telemetry Server
```

## Prerequisites

- A registered domain name (e.g., `myrobotaxi.app`) with DNS control
- HTTPS hosting capability for the `.well-known` public key endpoint
- `openssl` CLI tool (1.1.0 or later)
- `curl` and `jq` for API calls
- A Tesla Developer account at https://developer.tesla.com
- A Tesla vehicle with firmware 2024.26 or later

## Step 1: Generate Cryptographic Keys

Tesla requires an EC key using the `secp256r1` (aka `prime256v1`) curve. This is not negotiable -- RSA and other EC curves are rejected.

```bash
# Generate all certificates for local development.
./scripts/generate-certs.sh myrobotaxi.app

# Generated files:
#   certs/server.key      — EC private key (KEEP SECRET)
#   certs/public-key.pem  — Public key (host at .well-known endpoint)
#   certs/server.crt      — Self-signed server cert (dev only)
#   certs/client.key      — Test client key (for simulator)
#   certs/client.crt      — Test client cert (for simulator)
```

**Important:** The private key (`server.key`) must never be committed to version control or shared. It is used for mTLS handshake with Tesla vehicles and for signing API requests.

## Step 2: Host the Public Key

Tesla validates your application by fetching the public key from a well-known URL. The URL format is strict and must be exactly:

```
https://<your-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem
```

For MyRoboTaxi:
```
https://myrobotaxi.app/.well-known/appspecific/com.tesla.3p.public-key.pem
```

### Hosting on Vercel (MyRoboTaxi uses this)

If your domain is hosted on Vercel (as the MyRoboTaxi Next.js app is), place the public key file in the Next.js `public/` directory:

```
my-robo-taxi/public/.well-known/appspecific/com.tesla.3p.public-key.pem
```

Verify it is accessible:
```bash
curl -s https://myrobotaxi.app/.well-known/appspecific/com.tesla.3p.public-key.pem
# Should output the PEM-encoded public key
```

### Common Gotchas

- **www vs non-www:** The domain in the public key URL must exactly match the domain used for registration. If you register `myrobotaxi.app`, the key must be at `myrobotaxi.app`, NOT `www.myrobotaxi.app`. Mismatches cause silent certificate failures.
- **Content-Type:** The endpoint should serve the file with `Content-Type: application/x-pem-file` or `text/plain`. Most hosting platforms handle this correctly for `.pem` files.
- **HTTPS required:** The endpoint must be HTTPS. HTTP will not work.

## Step 3: Register a Tesla Developer Application

1. Go to https://developer.tesla.com and sign in with your Tesla account.
2. Create a new application:
   - **App name:** MyRoboTaxi (or your app name)
   - **Description:** Real-time vehicle telemetry for ride tracking
   - **Website:** https://myrobotaxi.app
   - **Allowed origins:** https://myrobotaxi.app
   - **Allowed redirect URIs:** https://myrobotaxi.app/api/auth/callback/tesla
3. Note your **Client ID** and **Client Secret**.
4. Under **API Access**, request access to the Fleet Telemetry scope.

**Processing time:** Tesla's review of your application and CSR can take days to weeks. Plan accordingly and submit early.

## Step 4: Register with Fleet API

After your developer application is approved, register your public key with Tesla's Fleet API. This tells Tesla to trust your server's certificate for mTLS connections.

### 4a. Obtain a Partner Auth Token

Exchange your client credentials for a Partner Auth Token:

```bash
curl -s -X POST "https://auth.tesla.com/oauth2/v3/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=<YOUR_CLIENT_ID>" \
  -d "client_secret=<YOUR_CLIENT_SECRET>" \
  -d "scope=openid vehicle_device_data vehicle_cmds" \
  -d "audience=https://fleet-api.prd.na.vn.cloud.tesla.com" | jq .
```

Save the `access_token` from the response. This is your Partner Auth Token.

### 4b. Register Your Application

Call the Fleet API `/register` endpoint. This must be done once per region:

```bash
curl -s -X POST "https://fleet-api.prd.na.vn.cloud.tesla.com/api/1/partner_accounts" \
  -H "Authorization: Bearer <PARTNER_AUTH_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "domain": "myrobotaxi.app"
  }' | jq .
```

Tesla will fetch and verify your public key from the `.well-known` endpoint during this call. If it fails, check that the public key is accessible and correctly formatted.

### Regional API Endpoints

Tesla Fleet API has different base URLs per region:

| Region | Base URL |
|--------|----------|
| North America | `https://fleet-api.prd.na.vn.cloud.tesla.com` |
| Europe | `https://fleet-api.prd.eu.vn.cloud.tesla.com` |
| China | `https://fleet-api.prd.cn.vn.cloud.tesla.com` |

Use the endpoint matching the vehicle owner's region.

## Step 5: Vehicle Owner Pairs Virtual Key

Each vehicle owner must explicitly grant your application access by pairing a virtual key. This is a one-time action per vehicle.

Direct the vehicle owner to:
```
https://tesla.com/_ak/myrobotaxi.app
```

This opens the Tesla app and prompts the owner to:
1. Select which vehicle(s) to pair
2. Approve the key pairing request
3. Tap their key card on the center console (required for security)

**Important notes:**
- The vehicle must be nearby and awake for key pairing.
- Max 5 third-party apps per vehicle.
- The owner can revoke access at any time in the Tesla app.

## Step 6: Configure Fleet Telemetry

Push the telemetry configuration to the vehicle using the Fleet API. This tells the vehicle which fields to stream and where to send them.

```bash
# Using the provided script:
./scripts/push-fleet-config.sh \
  --vin <VEHICLE_VIN> \
  --token <PARTNER_AUTH_TOKEN>

# Preview the payload without sending:
./scripts/push-fleet-config.sh \
  --vin <VEHICLE_VIN> \
  --token <PARTNER_AUTH_TOKEN> \
  --dry-run
```

### Manual API Call

```bash
curl -s -X POST \
  "https://fleet-api.prd.na.vn.cloud.tesla.com/api/1/vehicles/<VIN>/fleet_telemetry_config" \
  -H "Authorization: Bearer <PARTNER_AUTH_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "vins": ["<VIN>"],
    "config": {
      "hostname": "myrobotaxi.app",
      "port": 443,
      "ca": null,
      "fields": {
        "VehicleSpeed": {"interval_seconds": 5},
        "Location": {"interval_seconds": 5},
        "Gear": {"interval_seconds": 5},
        "Soc": {"interval_seconds": 30},
        "Odometer": {"interval_seconds": 60}
      },
      "alert_types": ["service"],
      "prefer_typed": true
    }
  }' | jq .
```

### Configuration Notes

- **interval_seconds:** How often the field is emitted (minimum effective ~0.5s due to batching).
- **Emission rule:** A field is emitted only when BOTH conditions are met: the interval has elapsed AND the value has changed since the last emission.
- **prefer_typed:** Set to `true` to receive typed protobuf values (float, int, enum) instead of strings where possible. Older firmware may still send strings.
- **ca:** Set to `null` for publicly trusted certificates (Let's Encrypt). Only set if using a custom CA.
- **exp:** Config expiration as Unix timestamp. Set to 30 days from now.

### Sync Verification

After pushing the config, check the sync status:

```bash
curl -s -X GET \
  "https://fleet-api.prd.na.vn.cloud.tesla.com/api/1/vehicles/<VIN>/fleet_telemetry_config" \
  -H "Authorization: Bearer <PARTNER_AUTH_TOKEN>" | jq .
```

Look for `"synced": true` in the response. Sync can take minutes to hours depending on the vehicle's state (awake vs asleep). The vehicle must be online to download the config.

## Step 7: Start the Telemetry Server

The telemetry server must:
1. Listen on port 443 with mTLS enabled
2. Present the server certificate signed by a trusted CA (or self-signed for dev)
3. Require client certificates from vehicles
4. Accept WebSocket upgrade requests

```bash
# Development mode with self-signed certs:
TLS_CERT_FILE=./certs/server.crt \
TLS_KEY_FILE=./certs/server.key \
go run ./cmd/telemetry-server --config configs/default.json
```

For production, use Let's Encrypt certificates and ensure your server is accessible at `myrobotaxi.app:443`.

## Production TLS with Let's Encrypt

Self-signed certificates work for development but Tesla vehicles in production require a certificate from a trusted CA. Let's Encrypt is free and widely used.

```bash
# Obtain a Let's Encrypt certificate:
certbot certonly --standalone -d myrobotaxi.app

# Certificates are stored at:
#   /etc/letsencrypt/live/myrobotaxi.app/fullchain.pem  (server cert)
#   /etc/letsencrypt/live/myrobotaxi.app/privkey.pem    (private key)
```

**Let's Encrypt certificates expire every 90 days.** You must:
1. Set up automatic renewal: `certbot renew --deploy-hook "systemctl restart telemetry-server"`
2. After renewal, re-push the fleet_telemetry_config to all vehicles.
3. Monitor expiry with: `./scripts/check-cert-expiry.sh --certs-dir /etc/letsencrypt/live/myrobotaxi.app --warn-days 14`

## Troubleshooting

### Vehicle not connecting

| Symptom | Cause | Fix |
|---------|-------|-----|
| No connection at all | Vehicle asleep | Wake vehicle via Tesla app, wait 2-5 minutes |
| `synced: false` persists | Vehicle not downloading config | Ensure vehicle is online; try re-pushing config |
| TLS handshake failure | Certificate mismatch | Check domain in cert matches hostname in config exactly (www vs non-www) |
| Connection drops immediately | Wrong key type | Must be EC key with secp256r1 curve, NOT RSA |
| 403 on config push | Virtual key not paired | Owner must visit `https://tesla.com/_ak/myrobotaxi.app` |
| 401 on config push | Expired auth token | Re-authenticate to get a new Partner Auth Token |

### Certificate issues

```bash
# Verify the server certificate:
openssl x509 -in certs/server.crt -noout -text

# Check certificate expiry:
./scripts/check-cert-expiry.sh

# Verify the key curve is correct:
openssl ec -in certs/server.key -noout -text 2>&1 | grep "ASN1 OID"
# Should show: ASN1 OID: prime256v1

# Test the public key endpoint:
curl -sI https://myrobotaxi.app/.well-known/appspecific/com.tesla.3p.public-key.pem
```

### Data format surprises

- **Numeric fields as strings:** Many fields (VehicleSpeed, Odometer, InsideTemp) arrive as `string_value` on older firmware. The decoder handles this automatically.
- **Location uses a special type:** Location fields use `locationValue` with nested `latitude`/`longitude`, not a regular value.
- **SelfDrivingMilesSinceReset:** Only available on HW4 vehicles. Returns no data on older hardware.
- **Buffer capacity:** Vehicles buffer up to 5000 messages. If the server is down, the vehicle will replay buffered messages on reconnect, but oldest messages are dropped if the buffer fills.

## Architecture Reference

For details on how telemetry data flows through the server, see:
- [Architecture Overview](./architecture.md)
- [Data Flow Documentation](./data-flow.md)
