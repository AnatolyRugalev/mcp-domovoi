# domovoi

A minimal MCP server that gives an AI agent file and shell access to a single
Linux machine. One small static binary, a handful of tools, streamable HTTP, bearer-token
auth. Named after the Slavic house spirit that quietly keeps the household running.

Run one instance per machine. Point an MCP client at it directly, or place a
gateway in front to federate a fleet of machines as named targets. It is
deliberately minimal — no file search, no process management, no resources or
prompts, no directory listing (use `run_command` with `ls`), no TLS (put it on a
trusted network and terminate TLS at a reverse proxy or gateway).

## Tools

| Tool | Description |
|---|---|
| `read_file` | Read a file with `cat -n`-style line numbers. `offset` (1-based line) and `limit` (default 2000 lines) page through large files; reads are capped at 5 MB. Binary files are rejected. |
| `write_file` | Write full file content, creating parent directories and overwriting existing files. |
| `edit_file` | Exact-string replacement: `old_string` must be unique unless `replace_all` is set. Returns the replacement count and a unified-diff snippet. |
| `run_command` | Run a shell command via `bash -lc` (or `sh -c`). Structured result: `stdout`, `stderr`, `exit_code`, `duration_ms`, `timed_out`. Timeout defaults to 60 s (max 600 s); on timeout the whole process group is killed. Output keeps the last 100 KB per stream. A non-zero exit code is a normal result, not an error. |
| `server_info` | Report this instance's own identity: `version`, `name`, `os`, `arch`, `go_version`, `executable`, and whether passwordless sudo is available. Handy for confirming which machine and version an agent is talking to. |
| `self_update` | Download a release from GitHub, verify its checksum, replace the running binary in place, and re-exec onto it. Defaults to the latest release; pass `version` to pin a tag or `restart: false` to install without restarting. See [Self-update](#self-update). |

Every tool also takes an optional `sudo` boolean. When set, domovoi re-executes
itself under `sudo` and performs that one operation as root (see
[Elevation with sudo](#elevation-with-sudo)). The tool semantics otherwise follow
the conventions of common agent file/shell tools, so agents generally know how to
use them without special instructions.

## Install

Per-user systemd service, no root required:

```sh
curl -fsSL https://raw.githubusercontent.com/AnatolyRugalev/mcp-domovoi/main/install.sh | sh
```

On the first run this downloads the latest release for your architecture
(checksum verified), installs the binary under `~/.local/bin`, writes
`~/.config/domovoi/domovoi.env` with a freshly **generated token** (printed once
at the end), installs a user systemd unit, starts the service, and enables
lingering so it keeps running after you log out. Re-running the same command
updates the binary and restarts the service, leaving your config untouched.

For a system-wide service instead, pipe into `sudo sh`: the binary goes to
`/usr/local/bin`, config to `/etc/domovoi`, and the service runs as a dedicated
`domovoi` user.

Overrides: `DOMOVOI_VERSION=v0.2.0` pins a release, `DOMOVOI_MODE=user|system`
forces the install mode, `DOMOVOI_USER=<name>` sets the service user for a
system install.

Check it's up:

```sh
curl -s http://127.0.0.1:8811/healthz   # -> domovoi <version>
journalctl --user -u domovoi -f          # tool-call log (add --user only for a user install)
```

## Configuration

Flags, each with a `DOMOVOI_*` environment fallback (the env file the installer
writes uses the env-var form):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--token` | `DOMOVOI_TOKEN` | *(required)* | Bearer token; the server refuses to start without one. |
| `--listen` | `DOMOVOI_LISTEN` | `0.0.0.0:8811` | Listen address. |
| `--path` | `DOMOVOI_PATH` | `/mcp` | URL path of the MCP endpoint. Set to something like `/mcp-<random>` as a secret-path fallback for clients that cannot send headers. |
| `--name` | `DOMOVOI_NAME` | *(system hostname)* | Human name for this machine. It is woven into the server instructions and every tool description so the agent knows it is operating on **this remote host**, not its own local environment (see [Machine identity](#machine-identity)). |
| `--allowed-dirs` | `DOMOVOI_ALLOWED_DIRS` | `/` | Colon-separated absolute path prefixes the **file tools** may touch, enforced after symlink resolution so `..` and symlink tricks cannot escape. |

`GET /healthz` responds `200` with the version and requires no auth.

### Machine identity

An agent talking to domovoi through a gateway can easily assume the filesystem
and shell it sees are its *own* local machine, and start editing the wrong host.
To prevent that, domovoi advertises the machine's name (from `--name`, defaulting
to the hostname) in two places every MCP client surfaces to the model: the
server-level `instructions` and each tool's description ("Read a file from the
remote host `yaga` (not your local machine)…"). Give each instance a
recognisable `--name` and the agent will address them as the distinct remote
machines they are.

## Security model

Be deliberate about this — domovoi hands an agent real control of the host.

- **Auth.** Every MCP request must carry `Authorization: Bearer <token>`
  (constant-time comparison; `401` otherwise). There is no TLS; run domovoi on a
  trusted network and terminate TLS upstream.
- **The service user is the boundary.** `--allowed-dirs` restricts only the file
  tools. `run_command` is **not** restricted by it — a command can do anything
  the user running the service can. Choose that user according to how much you
  trust the agent: a normal user for a scoped blast radius, or grant it
  passwordless `sudo` (or run the service as root) if the agent is meant to
  administer the machine.
- **Elevation is all-or-nothing.** If the service user has passwordless `sudo`,
  the `sudo` flag lets the agent act as root — there is no per-path or per-command
  narrowing beyond what your `sudoers` policy allows. Grant it only when the agent
  is meant to administer the machine.
- **Token handling.** The installer generates a random token and stores it
  `chmod 600`. Treat it like an SSH key.

### Elevation with sudo

Passing `sudo: true` to any tool makes domovoi re-execute *itself*
(`sudo -n domovoi worker ...`) and proxy that single tool call to the elevated
copy over stdio. The root worker runs the exact same Go code — line numbering,
UTF-8 checks, diff snippets, and the `--allowed-dirs` allowlist all still apply —
so the only thing that changes is the effective user. Nothing is shelled out to
`cat`/`tee`, and elevated calls are tagged `(sudo)` in the log.

This requires **non-interactive** sudo for the service user (`sudo -n` must not
prompt). If sudo would ask for a password, the call fails with the captured sudo
error instead of hanging. With no sudo configured, simply never set the flag and
domovoi behaves as an unprivileged file/shell server.

## Self-update

`self_update` upgrades a running instance without shelling out to the installer.
It resolves the target release (the latest, or the `version` tag you pass),
downloads that architecture's archive from GitHub, verifies it against the
release `checksums.txt`, and atomically replaces the running binary. It then
**re-executes itself** onto the new binary — the process keeps the same PID and
environment, so a systemd unit stays `active` with no external restart, and the
token from the `EnvironmentFile` is preserved. The one visible effect is that the
MCP connection drops during the swap; reconnect to reach the new version. Pass
`restart: false` to stage the new binary and defer the switch to the next
restart.

Replacing the binary needs write permission on it and its directory. The default
**per-user install** (`~/.local/bin/domovoi`, owned by the service user) satisfies
this. A **system install** puts the binary under `/usr/local/bin` owned by root
while the service runs as an unprivileged `domovoi` user, so `self_update` cannot
replace it — update those hosts by re-running the install script as root instead.

## Connecting a client

Point any MCP client that speaks streamable HTTP at
`http://<host>:8811/mcp` with an `Authorization: Bearer <token>` header. To try
it interactively:

```sh
npx @modelcontextprotocol/inspector \
  --transport http \
  --server-url http://127.0.0.1:8811/mcp \
  --header "Authorization: Bearer <token>"
```

### Behind a gateway

To federate several machines, front them with a gateway such as
[agentgateway](https://agentgateway.dev) and add one target per machine. If the
gateway can inject a per-target `Authorization` header, use that and keep the
default `/mcp` path. For example, with agentgateway:

```yaml
mcp:
  targets:
  - name: domovoi-<machine>
    mcp:
      host: http://<machine-host>:8811/mcp
    policies:
      backendAuth:
        key:
          value: <the machine's token>
          location:
            header:
              name: authorization
              prefix: 'Bearer '
```

If your gateway or client cannot send headers, set `DOMOVOI_PATH=/mcp-<random>`
and use that secret path in the target URL as a fallback. The bearer token is
still required; a secret path alone is weaker because paths tend to leak into
logs.

## Building from source

Requires a recent Go toolchain.

```sh
make test    # unit + integration tests (starts a real HTTP server)
make build   # static cross-compile for linux/amd64 + linux/arm64 into dist/
make run     # run locally on 127.0.0.1:8811 with a throwaway token
```

Built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
(v1.6.1). No other runtime dependencies.

## License

MIT — see [LICENSE](LICENSE).
