# domovoi

A minimal fleet MCP server. One tiny static binary per machine, four tools,
streamable HTTP, bearer-token auth. Named after the Slavic house spirit that
quietly keeps the household running.

Domovoi exists so agents can edit files and run commands on any machine in a
homelab without shelling out over SSH: each machine runs a domovoi, and a
central MCP gateway (e.g. [agentgateway](https://agentgateway.dev)) federates
them as named targets.

Deliberately minimal — no file search, no process management, no resources or
prompts, no directory listing (use `run_command` with `ls`), no TLS (LAN-only;
the gateway fronts external access).

## Tools

| Tool | Description |
|---|---|
| `read_file` | Read a file with `cat -n`-style line numbers. `offset` (1-based line) and `limit` (default 2000 lines) page through large files; reads are capped at 5 MB. Binary files are rejected. |
| `write_file` | Write full file content, creating parent directories, overwriting existing files. |
| `edit_file` | Exact-string replacement, same contract as Claude Code's Edit tool: `old_string` must be unique unless `replace_all`. Returns the replacement count and a unified-diff snippet. |
| `run_command` | Run a shell command via `bash -lc` (or `sh -c`). Structured result: `stdout`, `stderr`, `exit_code`, `duration_ms`, `timed_out`. Timeout default 60 s, max 600 s; on timeout the whole process group is killed. Output keeps the last 100 KB per stream. Non-zero exit is a normal result, not an error. |

The tool semantics deliberately match Claude Code's local Read/Write/Edit/Bash
tools, so agents already know the conventions.

## Configuration

Flags, each with a `DOMOVOI_*` environment fallback:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--token` | `DOMOVOI_TOKEN` | *(required)* | Bearer token; the server refuses to start without one. |
| `--listen` | `DOMOVOI_LISTEN` | `0.0.0.0:8811` | Listen address. |
| `--path` | `DOMOVOI_PATH` | `/mcp` | URL path of the MCP endpoint. Set to `/mcp-<random>` as a fallback secret for clients that cannot send headers. |
| `--allowed-dirs` | `DOMOVOI_ALLOWED_DIRS` | `/` | Colon-separated absolute path prefixes the **file tools** may touch, enforced after symlink resolution (so `..` and symlink tricks can't escape). |

`GET /healthz` responds `200` with the version, no auth — for monitoring.

### Security model, honestly

- Every MCP request requires `Authorization: Bearer <token>` (constant-time
  comparison, 401 otherwise).
- `--allowed-dirs` restricts `read_file`/`write_file`/`edit_file` only.
  **`run_command` is not restricted by it** — commands can touch anything the
  unix user can. The unix user the service runs as is the real privilege
  boundary; pick it per machine (see the comment in `domovoi.service`).
- No TLS: run it on a trusted LAN and front external access with a gateway.

## Install on a new machine

One-liner (installs or updates; requires root):

```sh
curl -fsSL https://raw.githubusercontent.com/AnatolyRugalev/mcp-domovoi/main/install.sh | sudo sh
```

On first run it downloads the latest release for your architecture (checksum
verified), installs `/usr/local/bin/domovoi`, creates the `domovoi` service
user, writes `/etc/domovoi/domovoi.env` with a **generated token** (printed
once at the end), installs the systemd unit, and starts the service. On later
runs it just replaces the binary and restarts — config is never touched.

Overrides: `sudo DOMOVOI_VERSION=v0.2.0 sh` pins a version,
`sudo DOMOVOI_USER=root sh` picks the service user for a first install.

### Manual install

1. Build (or grab `dist/` binaries from a release):

   ```sh
   make build   # dist/domovoi-linux-amd64 and dist/domovoi-linux-arm64, static
   ```

2. Copy things over (adjust arch):

   ```sh
   scp dist/domovoi-linux-amd64 machine:/tmp/domovoi
   ssh machine
   sudo install -m 755 /tmp/domovoi /usr/local/bin/domovoi
   sudo useradd -r -m -s /usr/sbin/nologin domovoi   # or pick an existing user
   sudo mkdir -p /etc/domovoi
   sudo cp domovoi.env.example /etc/domovoi/domovoi.env
   sudo chmod 600 /etc/domovoi/domovoi.env
   sudoedit /etc/domovoi/domovoi.env                 # set DOMOVOI_TOKEN=$(openssl rand -hex 32)
   sudo cp domovoi.service /etc/systemd/system/domovoi.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now domovoi
   curl -s http://localhost:8811/healthz             # -> domovoi <version>
   ```

Tool calls are logged to stdout (tool, path/command, outcome, duration), so
`journalctl -u domovoi -f` shows what agents are doing.

## Adding a machine to agentgateway

Agentgateway's `backendAuth` policy can inject the `Authorization` header on
a per-target basis, so header auth is the primary mechanism; keep `--path`
at `/mcp`. Under `mcp.targets` in `compose/agentgateway/config/config.yaml`:

```yaml
mcp:
  targets:
  - name: domovoi-<machine>
    mcp:
      host: http://<machine-ip>:8811/mcp
    policies:
      backendAuth:
        key:
          value: <the machine's DOMOVOI_TOKEN>
          location:
            header:
              name: authorization
              prefix: 'Bearer '
```

Fallback for gateways/clients that cannot inject headers: set
`DOMOVOI_PATH=/mcp-<random>` on the machine and use the secret path in the
target `host` (same pattern as a tokenized home-assistant webhook URL). The
bearer token is still required unless you front it with something that adds
the header — path secrecy alone is weaker (paths leak into logs), so prefer
`backendAuth`.

## Development

```sh
make test    # unit + integration tests (starts a real HTTP server)
make run     # run locally on 127.0.0.1:8811 with token "dev-token"
make build   # static cross-compile for linux/amd64 + linux/arm64
```

Smoke test with the MCP inspector:

```sh
npx @modelcontextprotocol/inspector \
  --transport http \
  --server-url http://127.0.0.1:8811/mcp \
  --header "Authorization: Bearer dev-token"
```

### Releasing

Releases are semver tags built by [goreleaser](https://goreleaser.com) in CI:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The `release` workflow runs the tests, cross-compiles static binaries for
linux/amd64 and linux/arm64, and publishes tarballs (binary + systemd unit +
env example) with a `checksums.txt` that `install.sh` verifies against.
The `ci` workflow (push/PR to main) runs vet, tests, a static-build check,
`goreleaser check`, and shellcheck on `install.sh`.

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
(pinned to v1.5.0, spec 2025-11-25). No other dependencies.
