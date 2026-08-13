# ADR-008: API token format and SHA-256 storage

Status: Accepted (2026-08-13)

## Decision

Bearer tokens `flp_<id8>.<secret>` where the secret is 32 random bytes (base64url). Config stores `(id, name, sha256(secret), permissions)`; lookup is O(1) by `id8`; comparison uses `crypto/subtle.ConstantTimeCompare`.

No KDF (bcrypt/argon2): those defend low-entropy passwords; a 256-bit random secret is not brute-forceable, and SHA-256 preimage resistance suffices — while keeping verification O(1) and dependency-free.

`fleetplane token new` generates locally and prints the secret once plus the config snippet. Rotation = config edit + restart (SIGHUP reload is a deferred stretch).
