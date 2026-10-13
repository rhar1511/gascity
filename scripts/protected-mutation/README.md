# Protected Beads revocation refresh

This optional Linux host service refreshes the signed revocation snapshot
consumed by a Beads backend with protected mutation support. It requires
Python 3 and the system `cryptography` package with Ed25519 support.
It does not configure lifecycle admission or start a controller.

Each instance reads one root-owned authority directory at
`/etc/gascity/protected-mutation/<scope>/`. That directory contains:

- `authority.json`: schema version 1, exact audience, project ID and database,
  plus the `keyring_file` and `revocations_file` names.
- The named keyring: schema version 1, a positive generation, and public
  keys with explicit issuer, key ID and purpose.
- `revoked-replay-ids.json`: the explicit JSON array of revoked replay IDs.
  A missing file fails the refresh; it never means an empty denylist.

The separate root-owned mode 0600 private key is
`/etc/gascity/protected-mutation-keys/<scope>-revocations.pem`. Its public key
must occur exactly once in the keyring, with purpose
`protected_mutation_revocation`. Never reuse a permit, admission, acceptance
or recovery key for revocations. All authority path components must be
root-owned, free of symlinks, and unwritable by group or others.

The refresher preserves the denylist, signs the backend's domain-separated
canonical payload, and atomically replaces only the named snapshot. Snapshots
last four minutes; the example timer refreshes every minute. A failed refresh
must be treated as an operational failure: the backend rejects stale evidence.
Changes to the denylist or keyring should use the same `.refresh.lock` and
atomic replacement protocol. Coordinate keyring generation changes with all
backend readers.

Before deployment, install the reviewed script as root-owned mode 0755 at
`/usr/local/libexec/gascity/refresh_protected_revocations.py`, provision the
scope's authority and key, and install the example system service and timer.
Run and verify an initial refresh before enabling the timer or admitting
protected mutations. Installation and activation require an operator-reviewed
host plan, including rollback. These files alone do not provision or activate
anything.

Run the portable payload checks from the repository root:

```sh
python3 -m unittest discover -s scripts/protected-mutation -p 'test_*.py'
systemd-analyze verify --man=no scripts/protected-mutation/gascity-beads-revocations@.service scripts/protected-mutation/gascity-beads-revocations@.timer
```

The unit tests cover payload binding, signature validity, explicit revocations,
canonical JSON, and rejection of invalid identities or shared-purpose keys.
Qualification against the actual backend verifier and the root-owned filesystem
installation are separate deployment checks.
