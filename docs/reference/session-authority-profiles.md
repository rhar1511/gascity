# Session authority profiles

Gas City can protect provider `permission_mode` changes with a dedicated,
host-owned signing authority. The controller recognizes four generic profile
identifiers: `design`, `router`, `worker`, and `operator`. A profile name does
not grant access by itself. A trusted grant must also name the exact provider
mode and the controller verifies both together.

## Activation

Set `GC_SESSION_AUTHORITY_TRUST_FILE` in the controller host environment to an
absolute path containing the public trust configuration. The controller reloads
the file for each transition and each launch. Removing an authority therefore
blocks later launches that depend on it. City configuration, packs, session
metadata, labels, and prompts cannot select the trust file or add a signer.

When the variable is absent, the existing permission-mode behavior remains in
place. This supports a staged rollout without changing live sessions merely by
installing a new controller binary. Once the variable is set, create-time
permission-mode overrides are refused and the transition endpoint requires the
signed fields below.

The trust file has this shape:

```json
{
  "keys": [
    {"key_id": "session-authority-1", "public_key": "<base64-ed25519-public-key>"}
  ],
  "authorities": [
    {
      "key_id": "session-authority-1",
      "issuer": "operations",
      "subject": "ricky",
      "profiles": ["design", "router", "worker", "operator"]
    }
  ],
  "revoked_authorization_ids": []
}
```

Private keys stay outside Gas City. The file must be absolute, regular,
non-symlinked, and not writable by group or other users. Grants may be valid for
at most five minutes. Add an accepted authorization ID to
`revoked_authorization_ids` to block it at later launch checks.

## Transition binding

`POST /v0/city/{city}/session/{id}/permission-mode` accepts the provider mode
plus `authority_profile`, `expected_generation`,
`effective_config_sha256`, and `authorization`. The signed grant binds:

- city and canonical session ID;
- exact execution generation;
- exact effective loaded configuration identity;
- previous and requested profiles;
- exact provider permission-mode value;
- signer identity, authorization ID, token ID, and validity window.

The controller resolves the session, generation, current profile, provider
schema, and loaded configuration itself. It never treats request labels or
prompt text as authority.

An accepted transition updates the permission-mode override, current profile,
signed authorization, and append-only transition history in one conditional
session-row write. Retrying the exact accepted authorization is idempotent.
Reuse after the stored state changes is rejected. Denied protected transitions
are also appended with a reason code.

The generic `gc bd` passthrough refuses writes to the controller-owned profile,
authorization, and transition-history keys. While authority enforcement is
active, it also refuses generic edits to `template_overrides` and
`opt_permission_mode`; those changes must use the signed transition endpoint.

Before applying the provider option at launch, both the API runtime resolver
and every controller launch path, including direct `gc session attach`, reload
host trust and verify the stored signature against the current session
generation, configuration identity, profile, and final effective mode. A work
bead's one-shot `opt_permission_mode` cannot replace or introduce an unsigned
mode. Protected malformed override metadata, missing proof, stale scope, and
newly untrusted records fail closed before runtime start. Other one-shot
provider options keep their existing behavior.
