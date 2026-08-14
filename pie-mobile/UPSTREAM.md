# Orca upstream snapshot

- Source: `/Users/jikime/Dev/Private/orca`
- Revision: `838259e594bf805b40f3070dfcbcd3c37ee9db57`
- Imported: 2026-07-22
- License: MIT, Lovecast Inc. (`upstream/LICENSE`)

## Copied source

- Complete `mobile/` application
- Complete `src/shared/` contracts used by mobile
- `src/main/runtime/rpc/`
- `src/main/runtime/relay/`
- Runtime pairing registry, keypair, and server wiring files
- Desktop mobile IPC and renderer pairing components
- `platform/apps/relay/` reference implementation

`upstream/mobile` was unmodified in the source worktree at import time and matches the revision
above. The broader Orca worktree contained unrelated uncommitted collaboration changes, so this
snapshot was exported from the named Git revision instead of copying unrelated dirty files.

## Update policy

Treat `upstream/` as a vendored, read-only tree. Refresh it from a reviewed Orca revision, record the
new revision here, and run both upstream mobile tests and adapter integration tests. Put all
cli-relay-specific behavior under `adapter/` so upstream diffs remain reviewable.

## Relay boundary

The snapshot includes two distinct relay implementations:

- `src/main/runtime/relay/` and the mobile `src/transport/mobile-relay-*` files implement the
  production mobile Director/Cell client protocol.
- `platform/apps/relay/` is an opaque session relay with a different wire contract. It is useful
  reference code, but it is not a drop-in implementation of the mobile Director/Cell endpoints.

Consequently, direct LAN pairing can be built entirely from this snapshot. Internet relay requires
either the missing Director/Cell service or a compatibility server that implements its published
client-side contract.
