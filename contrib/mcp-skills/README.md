# Optional Skills over MCP adapter

This fork-owned adapter publishes selected Git-managed skill directories over
MCP. It does not change `gc`, launch a service automatically, replace native
skill materialization, or run scripts. Beads remains the work and memory authority.

The implementation follows the [Skills Over MCP working group](https://modelcontextprotocol.io/community/working-groups/skills-over-mcp)
and its final [Skills extension](https://github.com/modelcontextprotocol/ext-skills).
It uses released `modelcontextprotocol/go-sdk` v1.8.0 for the base protocol,
stdio and Streamable HTTP; `skills/list` and `skills/get` use the SDK's public
custom-method API, without depending on the unmerged SDK Skills helpers.

[Issue #264](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/264)
concerns `_meta` clarity, not the Skills design (SEP-2640). Protocol metadata
stays in the SDK's `_meta` envelope; skill frontmatter and manifests are ordinary
result fields. Arbitrary frontmatter fields pass through unchanged. Metadata
is not authorization and instructions cannot grant themselves permissions.

## Build and opt in

From the repository root:

```sh
go build -o /absolute/path/to/gascity-mcp-skills ./contrib/mcp-skills
/absolute/path/to/gascity-mcp-skills --catalog team=/absolute/path/to/pack/skills
```

Each selected root contains named skill directories with YAML-frontmatter
`SKILL.md` files. Catalog namespaces must be distinct lowercase names. Multiple
roots are supplied by repeating `--catalog`; the same skill name in different
namespaces remains distinct. Use a reviewed, clean checkout pinned to a commit.
The adapter snapshots bytes at startup; restart it to publish a new revision.
No live file reads, polling, memory database, or catalog installation occurs.

Copy `gascity-skills.toml.example` as `gascity-skills.toml` into the desired
existing Gas City MCP catalog,
replace the absolute paths, and use existing `gc mcp list --agent NAME` to
inspect projection. Leaving the file out disables the adapter. The example is
not loaded from this contrib directory. Provider support for MCP configuration
does not prove that the provider understands the Skills extension.

Modern clients discover `io.modelcontextprotocol/skills` in `server/discover`.
One `skills/list` page supplies full frontmatter and every file's SHA-256 digest
and size; it eliminates one `skills/get` per listed skill. Files are fetched
lazily with `resources/read`; binary files use base64. Pagination keeps each
skill manifest atomic. `resources/list` exposes files for generic MCP clients.
The SDK supports legacy initialization too; native local skills remain the
fallback where the host cannot load MCP skills. Optional directory traversal is
not advertised. This server exports no tools or arbitrary CLI execution.

## Optional remote access

HTTP is disabled unless `--listen` is supplied. Bind to loopback and expose it
through an authenticated TLS proxy (for example a configured Tailscale proxy):

```sh
# Supply a random secret from your secret manager, not a committed config.
export GC_SKILLS_MCP_TOKEN='<random secret of at least 32 bytes>'
/absolute/path/to/gascity-mcp-skills \
  --catalog team=/absolute/path/to/pack/skills --listen 127.0.0.1:8765
```

The endpoint is `/mcp`; every request needs `Authorization: Bearer <secret>`.
Browser Origins are denied unless explicitly allowed with `--origin`.
Use the modern protocol's version/method/name headers; the SDK checks them.
Requests are limited to 1 MiB with bounded HTTP timeouts. No TLS, OAuth issuance,
public listening address or proxy configuration is installed by this adapter.
Catalog responses have private cache scope, including discovery and resources.
Do not place credentials, private working files or unrelated assets inside a
published skill directory: **every** supporting file is part of its manifest.
Symlinks and special files anywhere in a catalog are refused; source reads are
confined with `os.Root`. Limits are 512 files/16 MiB per skill, 8192 source files,
1024 skills and 64 MiB of unique bytes per server snapshot. Nested skills have
separate entries; their files also belong to enclosing manifests.

## Host responsibility and validation

Skill identity is **host-assigned server identity plus URI**. Hosts must not
silently replace same-named local skills. Loading requires digest, size and
frontmatter verification plus the applicable user approval; reading `SKILL.md`
is only a resource read. Changed manifests revoke content-bound approvals.
Nested skills require their own consent. Digests prove consistency, not trust.
The server does not implement the host loader or claim those protections exist
in a client merely because it connects.

```sh
go test -race ./internal/mcpskills ./contrib/mcp-skills
go vet ./internal/mcpskills ./contrib/mcp-skills
```

Tests cover complete nested manifests, raw-byte snapshots, invalid frontmatter,
symlink rejection, size limits, SDK client discovery/get/read/pagination and
modern HTTP authentication, Origin and header checks. The working group's
[server conformance scenarios](https://github.com/modelcontextprotocol/conformance/pull/330)
were run from conformance commit `c37eec888e1c6ff140af79987a40008548b7cc5f`
against an isolated HTTP instance at protocol version `2026-07-28`: enumeration
32/32, manifest 6/6, and directory wire-schema 1/1 passed. Six directory-read
checks were skipped because that optional capability is not advertised. The
runner used a temporary loopback proxy to inject the bearer credential; direct
protection checks are owned by the local tests. These are server checks, not
certification of a host loader. No VM deployment or auto-activation is implied.
