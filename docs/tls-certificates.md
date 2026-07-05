# TLS Certificates — Topology, Renewal & Monitoring

`telemetry.myrobotaxi.app` serves **two independent TLS surfaces on one
hostname**. They use **different certificates, issued and renewed by
different systems.** Confusing them is what caused the 2026-07 outage
(MYR-188), so read this before touching certs.

```
                         telemetry.myrobotaxi.app  (CNAME → *.fly.dev, DNS at Dynadot)
                          │
   ┌──────────────────────┴───────────────────────┐
   │ Port 443  — Tesla mTLS                        │ Port 4443 — browser WS + REST
   │ Fly TCP passthrough (NO tls handler)          │ Fly terminates TLS
   │ the APP terminates TLS + reads client certs   │ app serves plain HTTP on :8080
   │ cert: TLS_CERT_B64 / TLS_KEY_B64 Fly secrets  │ cert: Fly-managed (fly certs)
   │ renewal: MANUAL (see §2)                       │ renewal: automatic once DNS-validated (§3)
   └───────────────────────────────────────────────┘
```

Both certs currently expire **2026-10-03**.

---

## 1. Why one hostname needs two renewal paths

- **Port 443 can't use Fly's managed cert.** It's a raw TCP passthrough so
  the app can complete the mTLS handshake and read each vehicle's client
  certificate. Fly never sees the bytes, so it can't terminate TLS there —
  the app must present its own cert from a secret.
- **Fly's managed cert (4443) can't validate over HTTP-01.** Fly's ACME
  HTTP-01 challenge is served on port 443, but 443 is the passthrough — the
  challenge can't be answered. **The Fly cert MUST use DNS validation** (§3).
- **The two ACME challenges must not collide.** A single-name cert for
  `telemetry.myrobotaxi.app` validates at `_acme-challenge.telemetry.myrobotaxi.app`
  — the same name Fly's DNS delegation uses. A CNAME (Fly) and a TXT
  (certbot) cannot coexist there. So the **app's 443 cert is a wildcard
  `*.myrobotaxi.app`**, which validates at the apex `_acme-challenge.myrobotaxi.app`
  — a different name that never clashes with Fly's.

---

## 2. Renew the port-443 mTLS cert (wildcard `*.myrobotaxi.app`)

The app cert is a Let's Encrypt wildcard delivered via the `TLS_CERT_B64` /
`TLS_KEY_B64` Fly secrets. It is **manual DNS-01** today (see the automation
gap in §4).

```bash
# 1. Issue/renew the wildcard (DNS-01 — the ONLY challenge that works here).
sudo certbot certonly --manual --preferred-challenges dns -d '*.myrobotaxi.app'

# 2. Certbot prints a TXT value. At Dynadot, set:
#      Subdomain: _acme-challenge   Type: TXT   Value: <printed value>
#    NOTE: apex "_acme-challenge", NOT "_acme-challenge.telemetry"
#    (that one is Fly's — see §3). Verify before continuing:
dig +short TXT _acme-challenge.myrobotaxi.app @ns1.dyna-ns.net

# 3. Push the new cert to Fly (files are root-only; read with sudo,
#    run fly as your own user so it uses your login):
fly secrets set -a my-robo-taxi-telemetry \
  TLS_CERT_B64="$(sudo base64 -i /etc/letsencrypt/live/myrobotaxi.app/fullchain.pem)" \
  TLS_KEY_B64="$(sudo base64 -i /etc/letsencrypt/live/myrobotaxi.app/privkey.pem)"
# Setting secrets triggers a rolling redeploy; start.sh decodes them to
# /tmp/certs/server.crt on boot.

# 4. Verify the served cert (should be the new NotAfter, mTLS port):
scripts/check-cert-expiry.sh --endpoint telemetry.myrobotaxi.app:443
```

Vehicles reconnect on their own as they next phone home; no fleet-config
re-push is needed because the hostname is unchanged.

---

## 3. Renew / fix the port-4443 Fly-managed cert

Normally automatic. If `fly certs show telemetry.myrobotaxi.app` is stuck on
`Issuing…` or expired, it's because HTTP-01 can't traverse the 443
passthrough. Move it to **DNS validation** (one-time; auto-renews after):

```bash
fly certs setup telemetry.myrobotaxi.app -a my-robo-taxi-telemetry
# Add the CNAME it prints at Dynadot:
#   _acme-challenge.telemetry.myrobotaxi.app  CNAME  telemetry.myrobotaxi.app.<hash>.flydns.net
# (Delete any stale _acme-challenge.telemetry TXT first — a CNAME can't
#  coexist with a TXT.)
fly certs check telemetry.myrobotaxi.app -a my-robo-taxi-telemetry   # nudges validation
scripts/check-cert-expiry.sh --endpoint telemetry.myrobotaxi.app:4443
```

---

## 4. Monitoring & alerting

- **EndpointCertMonitor** (`internal/telemetry/cert_endpoint_monitor.go`)
  TLS-dials every endpoint in `TLS_MONITOR_ENDPOINTS` and emits
  `telemetry_tls_endpoint_cert_expiry_days_remaining{endpoint}` +
  `telemetry_tls_endpoint_cert_reachable{endpoint}`. It reads the **served**
  leaf, so it sees the Fly 4443 cert that the older file-based monitor
  structurally cannot. Enable it in prod:
  `TLS_MONITOR_ENDPOINTS=telemetry.myrobotaxi.app:443,telemetry.myrobotaxi.app:4443`.
- **Alerts**: `deployments/alerts/tls-cert.rules.yml` pages at 30 / 14 / 7
  days remaining and on 15m of unreachability.
- **Ad-hoc / CI check**: `scripts/check-cert-expiry.sh --endpoint HOST:PORT`.

### Known gap — 443 cert auto-renewal (tracked, MYR-188)

The wildcard is `certbot --manual`, so it does **not** auto-renew. Until an
auth-hook / DNS-plugin flow (Dynadot API or acme.sh) that re-sets the Fly
secrets is wired, renewal is a **manual calendar task before 2026-10-03**.
The alert rules above are the safety net.
