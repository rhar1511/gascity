# Local Graphify source context

Graphify is a derived, local architecture index for Gas City source. It helps
with dependency, impact, and unfamiliar-code questions; source remains the
authority. Beads remains the authority for task status and dependencies.

## Use it on demand

For a task with an issue ID, read only the relevant Beads context first:

```bash
bd show <task-id>
bd dep tree <task-id>
```

Then ask one scoped source question:

```bash
scripts/graphify-harness.sh query "how does session startup reach the bead store?"
scripts/graphify-harness.sh path "internal/session" "internal/beads"
scripts/graphify-harness.sh affected "Config"
```

The query wrapper passes an explicit 1,200-token budget and never permits more
than 1,500. It prints the source revision and freshness receipt used for the
answer. Follow every returned path or symbol in the source before relying on
it. A missing CLI, missing graph, stale receipt, or query failure is reported
and the query falls back to a bounded `rg` search over source and Markdown.

## Build and refresh

```bash
scripts/setup-graphify.sh
scripts/graphify-harness.sh status
scripts/graphify-harness.sh update
```

Setup installs the pinned `graphifyy[sql,terraform]==0.9.37` CLI and extracts
code selected by the repository ignore policy with the AST-only `--code-only`
mode. Clustering keeps placeholder community labels and skips HTML
visualization. No documents, Beads data, or source are sent to an LLM.
`graphify-out/` contains the ignored graph and its JSON freshness receipt; it
is rebuildable local state.

Graphify can miss dynamic imports, generated code, configuration, and behavior
that is not represented in static syntax. If the graph is misleading, report
the graph issue separately and use `rg` plus direct source reads. Never treat a
missing Graphify node as proof that code is dead or unrelated.
