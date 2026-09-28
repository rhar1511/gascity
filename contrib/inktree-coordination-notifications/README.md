# Inktree coordination notification policy

This contribution supplies Inktree's consumer policy for the role-neutral
`internal/coordinationnotify` primitives. It maps known router reason strings
to the released coordination reason codes, assigns fixed neutral copy, and
keeps both `live_dispatch` and `notification_dispatch` off. The pack is replay
only; it defines no order, transport binding, worker, route, or delivery
action.

The generic package owns deterministic envelope construction, assertion and
record projections, fixed-template rendering, switch validation, and
same-epoch dedup receipts. This directory owns the Inktree reason vocabulary
and copy. Unknown reason strings and switch values fail closed.

## Offline evidence

The evidence generator writes deterministic reports for a synthetic
`req-syn-*` input, including an offline receipt fixture. It does not create a
Forgejo delivery receipt or run a live canary. No live dispatch or consumer
overlay is included in this pack. `evidence/verification.json` records which
checks ran in the implementation environment.

Regenerate the files from the repository root with:

```sh
go run ./contrib/inktree-coordination-notifications/cmd/evidence \
  --out contrib/inktree-coordination-notifications/evidence
```

Run focused tests with:

```sh
go test ./internal/coordinationnotify \
  ./contrib/inktree-coordination-notifications/...
```

The live consumer still needs a separately reviewed ordinary-pack adapter and
operator-managed identity/channel bindings. This contribution does not install
itself or alter a city.
