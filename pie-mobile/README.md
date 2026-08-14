# Pie Relay Mobile Stack

This directory keeps the vendored, proven mobile transport and Pie Relay integration code
together.

## Layout

- `upstream/`: a vendored source snapshot plus narrowly tracked Pie compatibility patches.
- `adapter/host-gateway/`: the Pie Relay host adapter. It uses the vendored WebSocket transport,
  per-device registry, pairing contract, and application-layer E2EE implementation.
- `UPSTREAM.md`: source revision, license, copied paths, compatibility labels, and update policy.

Internal `Orca` type names, storage keys, cryptographic labels, and legacy URL scheme remain where
changing them would break the imported wire protocol or existing installations. They are protocol
compatibility identifiers, not the public product brand. New user-visible text, assets, and links
use Pie Relay and `cookai.dev`.

## Connection modes

The direct same-LAN path is:

1. Start the host gateway on the PC.
2. Scan the printed `pierelay://pair` QR code with the Pie Relay mobile app.
3. The phone authenticates with a per-device token inside the application-layer E2EE channel.
4. The app opens a real PTY owned by the host gateway.

The remote path uses the Pie Relay server's Director/Cell-compatible `/v1` endpoints. A pairing
offer can carry both LAN and Relay candidates; endpoint supervision selects and recovers the usable
path without treating two paths to the same host as two independent hosts.

The Relay backend defaults to Azure. Set the single `PIE_RELAY_URL` variable on Desktop or the
standalone Gateway to a local or custom Relay when needed. The mobile app
does not keep a separate global Relay default—the selected Director/Cell URL is carried in the QR
pairing offer. See [`../docs/relay-url-configuration.md`](../docs/relay-url-configuration.md)
for precedence and examples.

The Host Gateway obtains its runtime identity from Relay `/v1/identity`, where the capability
token has already been verified. It no longer parses unsigned JWT payload data or hardcodes an
organization. The existing Director assignment v1 body remains unchanged, so current mobile apps
continue to accept `{ v, cellUrl, assignmentEpoch, lease }` without a schema update.

The Pie Relay Desktop app owns the gateway as a separate child process. Open **Pie Relay 모바일**
in the left sidebar, choose the address/start directory if needed, and click **모바일 호스트 시작**.
The panel renders the pairing offer as a QR code and lets the operator revoke each paired device.
Revocation updates the live `DeviceRegistry` and terminates that device's active connections.

## Run the mobile app

The app is an Expo development build, not an Expo Go app. On iOS, build and install the native
client once, then keep Metro running over the LAN address so a simulator or physical phone can load
the bundle:

```bash
cd pie-mobile/upstream/mobile
pnpm install --frozen-lockfile
pnpm exec expo run:ios --device
pnpm start --dev-client --lan
```

For Android, start an emulator or connect a device and use `pnpm android`, followed by the same
Metro command. `pierelay://pair?...` is the public scheme. `orca://pair?...` remains accepted only
so pairing links issued before the Pie migration continue to work.

## Run the LAN host gateway

```bash
cd pie-mobile/adapter/host-gateway
pnpm install --frozen-lockfile
pnpm start -- --advertise-host 192.168.0.10
```

Use the PC's address on the same Wi-Fi for `--advertise-host`. The gateway prefers port `6768` and
falls back to an available port using the vendored transport implementation. The selected fallback
is persisted and rebound first on later launches, so a paired phone keeps the same endpoint.

## Verify

```bash
(cd pie-mobile/upstream/mobile && pnpm test && pnpm typecheck && pnpm lint)
(cd pie-mobile/adapter/host-gateway && pnpm test && pnpm typecheck && pnpm build)
(cd desktop && npm test && npm run build)
(cd desktop/src-tauri && cargo check)
```

Native release approval still requires a physical-device run for LAN, remote Relay, network
handoff, background/resume, and pairing revocation. See `docs/release-readiness.md`.
