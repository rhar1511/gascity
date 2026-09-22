# Upstream Gas City sync — 2026-09-22

Compared the fork's `main` at `840befc50` with `gastownhall/gascity/main` at `42d46228d`. The upstream side is 146 commits ahead. The groups below summarize the behavior changes represented by that range; the commit IDs are primary source references in the repository history.

## Beads 1.3 and storage compatibility

- Gas City now supports the Beads 1.3 line, including the verified shared-database upgrade path, close guards, RC/final integration coverage, and permission/ownership evidence (`dea5e5867`, `9509c7f99`, `d45821663`, `82639ec57`, `84a4cf26b`).
- Local Dolt can run through the proxy endpoint by default, with live managed-port resolution, bounded idle connections, safer cleanup, and fail-closed behavior when rig enumeration is unavailable (`af7ad8a0f`, `849cb1a39`, `ca7b0e611`, `8304178b7`, `b2decffd6`).
- Doctor and status paths expose stronger evidence and refuse to claim healthy state when the store contradicts the reported result (`97632797b`, `70c3f0f88`, `49cf46f90`).

## Session and reconciler reliability

- Named-session lookup is batched during desired-state evaluation, while fresh sessions and adopted runtime instance tokens are preserved (`42d46228d`, `97dcdaad3`, `9877ab556`).
- Runtime liveness is bounded, explicit restarts are prompt and non-blocking, stale named-session phantoms can be recycled, and remote kills are guarded from destroying live subagents (`baf96bf44`, `fea5c10c8`, `fa8c704fb`, `ec4683a6b`).
- Pool sessions now retain durable session beads, concrete work directories, safer seat reuse, and coherent suspended/asleep metadata (`f7402bdd5`, `06d073fe6`, `6bb1c5586`, `516809154`).
- Tmux startup, teardown, orphan reaping, and process identity checks are more bounded and observable (`a74e4ca79`, `9ba881394`, `d6e867a6f`, `44bba224c`).

## Routing, dispatch, and workflow correctness

- One-shot pool steps route independently and fail loudly when continuation groups would be dropped (`61211b6da`, `6e9a24a55`).
- Formula lookup now searches all configured layers, role aliases resolve consistently, and claim-time work-branch resolution comes from the worker checkout (`799af8afd`, `99543af56`, `a4c0b9f62`).
- Dispatch and handoff paths retry recoverable rig-removal races, preserve live hook claims, and stamp failed-DAG diagnostics onto the domain parent (`4dac8847a`, `8d0f7db07`, `ec60eca7b`).

## Mail, nudge, and event safety

- Stale nudges are re-fenced or explicitly dropped, then revalidated at delivery time; duplicate notifications are collapsed and retention uses configured TTL (`bd8a9bb51`, `0ab33e5a3`, `c60a563ea`, `9cfb391f6`, `06b47b6af`).
- Event recording and SSE delivery are bounded, timestamp-preserving, and kept off bead stores where appropriate (`ad34febe7`, `3618fc23a`, `631929f41`).

## Supervisor, observability, and operator tooling

- Supervisor socket paths are protected from Unix path-length failures, status is fail-closed against contradictory store rows, and metrics honor injected clocks (`a76bff41c`, `1fa407dc2`, `49cf46f90`).
- Startup and launcher behavior is documented and tested more explicitly; `mayor-chat`, context-pressure advisories, and pack-skill materialization expand operator support (`cfb3a9cec`, `4e27de701`, `e10ddee75`, `6cc775654`, `9e2ec6b99`).
- The build and release surface gained side-by-side Bazel/Gazelle support, stronger CI gates, hook ownership chaining, and refreshed v1.4.1/v1.4.2 compatibility work (`d0c74873b`, `58ef17e3b`, `a963597f0`, `ebbb019f5`).

## Fork-only material carried forward

The sync branch preserves the fork's contributions selectively:

- Beads Canvas source, tests, optional dashboard activation, export helpers, and a freshly generated embedded bundle.
- The bounded Inktree HQ mirror pack under `contrib/inktree-hq-mirror/`.

Generated assets from the old fork commit were regenerated against current upstream source instead of replayed wholesale.
