# Portable Beads config and local runtime state

Beads supports a machine-specific `.beads/config.local.yaml` beside its
project `.beads/config.yaml`. Gas City reads that explicit local layer too.
Local values override portable defaults; nested Dolt settings inherit values
that the local layer leaves unset.

To keep a rig's existing tracked config portable, create the local file before
bootstrap or reload. An empty YAML mapping is enough to select the local
write layer:

```yaml
{}
```

Keep this machine-specific file out of source control. Gas City's canonical
config writer then updates the local file and leaves the portable file's bytes
unchanged. Without a local file, the existing single-file behavior continues.
The local file does not change the metadata database or project identity, and
does not transfer ownership of the backing store.

Gas City preserves local extensions and unions custom bead types from portable
defaults, local extensions, and its required types. It does not copy unrelated
portable settings into the local file. Endpoint values that must be cleared
use YAML nulls in the local layer so an old portable endpoint cannot reappear
through inheritance.

An unreadable or malformed explicit local layer, or a missing portable project
file alongside it, fails without repairing or rewriting either file. Legacy
malformed-config repair remains available only
for the single-file configuration. Resolve an invalid local file before
starting or reloading the city.
