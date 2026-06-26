# odino

CLI for Rome public-transport realtime arrivals, backed by the
[Roma Mobilità open data feeds](https://romamobilita.it/sistemi-e-tecnologie/open-data/)
(GTFS static + GTFS-Realtime). No API key required.

Each arrival row is tagged `LIVE` (realtime delay applied) or `SCHED`
(planned only). Ships an MCP server so AI agents can query the same data.

## Cache directory

`odino` keeps its parsed GTFS data (SQLite database, raw ZIP, MD5 sum) in
a local cache directory:

- `$ODINO_CACHE_DIR` if set, else
- `$XDG_CACHE_HOME/odino` if set, else
- `~/.cache/odino` (default on macOS and Linux).

The cache is refreshed lazily once a day. The static GTFS ZIP is re-parsed
only when its upstream MD5 changes, so daily refreshes are nearly free.

## Installation

### From source

```sh
git clone https://github.com/vlauciani/odino.git
cd odino
go build -o odino ./cmd/odino
```

### Via `go install`

```sh
go install github.com/vlauciani/odino/cmd/odino@latest
```

This drops the `odino` binary in `$(go env GOPATH)/bin` (usually `~/go/bin`).

### Docker

```sh
docker build -t odino:latest .
```

Multi-stage Alpine image, ~15 MB. Inside the container the cache path is
`/var/cache/odino`; bind-mount your host `~/.cache/odino` there to persist
the SQLite database across runs:

```sh
mkdir -p ~/.cache/odino
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest update
```

The `--user $(id -u):$(id -g)` flag matches the container process to your
host UID/GID, so files in `~/.cache/odino` end up owned by you (not root,
not the container's `odino` user). On macOS Docker Desktop you can usually
omit it; on Linux it is recommended.

## Usage

```
odino <command> [flags]

Commands:
  update                  Refresh the GTFS static cache.
  arrivals <stop>         Next arrivals at a stop (live + scheduled).
  vehicles                Live vehicle positions, optionally filtered by line.
  vehicle <vehicle_id>    Follow a specific vehicle across its remaining stops.
  alerts                  Active service alerts.
  stops search <query>    Search stops by name substring.
  stops nearby --lat --lon --radius [--route]
                          Stops within a radius (in metres) of a coordinate.
  routes                  List all routes.
  mcp                     Run the MCP server over stdio.

Global flags:
  --json                  Emit JSON instead of a table.

arrivals flags:
  --route <name>          Filter by route_short_name (e.g. 64).
  --limit <n>             Max rows (default 10).
  --window <minutes>      Look-ahead window (default 60).

vehicle flags:
  --to <stop_id>          Destination stop. Trims the upcoming list to end
                          at this stop and prints a summary line with ETA,
                          minutes remaining, and stops to go.
  --limit <n>             Max upcoming stops (0 = all). Ignored when --to is set.

stops nearby flags:
  --lat <deg>             Latitude (WGS84, decimal degrees). Required.
  --lon <deg>             Longitude (WGS84, decimal degrees). Required.
  --radius <metres>       Search radius in metres. Required.
  --route <name>          Keep only stops served by this line.
  --limit <n>             Max rows (default 10).
```

`<stop>` in `arrivals` is auto-detected: numeric → `stop_id`, otherwise a
case-insensitive substring match on `stop_name`. Times are reported in
the `Europe/Rome` timezone.

## Examples

### Native binary

```sh
# First-time setup: populate the cache (~30 MB download).
odino update

# Find a stop by street name fragment.
odino stops search Termini

# Stops within 300 m of a coordinate.
odino stops nearby --lat 41.853563 --lon 12.499133 --radius 300

# Same area, restricted to line 30.
odino stops nearby --lat 41.853563 --lon 12.499133 --radius 800 --route 30

# Next arrivals at a stop (by stop_id or name substring).
odino arrivals 77211
odino arrivals "Vittorio Emanuele/Argentina"

# Filter by line.
odino arrivals 77211 --route 64

# JSON output (for scripts and agents).
odino arrivals 77211 --route 64 --json | jq .

# Live position of every line-64 bus.
odino vehicles --route 64

# Follow a specific vehicle through every remaining stop on its current trip.
odino vehicle 888

# Same, but only up to a destination stop_id (ETA + stops-to-go summary).
odino vehicle 888 --to 74618

# Active service alerts.
odino alerts
```

### Docker

The host `~/.cache/odino` directory is mounted at `/var/cache/odino`
inside the container, so the GTFS cache persists across runs.

```sh
# Populate (or refresh) the cache from inside the container.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest update

# Next arrivals at C.so Vittorio Emanuele / Argentina (stop 77211), line 64.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest arrivals 77211 --route 64

# Stops within 250 m of a coordinate.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest stops nearby --lat 41.853563 --lon 12.499133 --radius 250

# Live vehicle positions for line 64.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest vehicles --route 64 --json

# Follow bus 888 until it reaches stop 74618 (ETA + stops to go).
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest vehicle 888 --to 74618
```

Handy shell alias:

```sh
alias odino='docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino odino:latest'
# Then: odino arrivals 77211 --route 64
```

## MCP server

`odino mcp` starts a Model Context Protocol server on stdio. Each CLI
subcommand is exposed as an MCP tool returning JSON.

Available tools: `arrivals`, `vehicles`, `vehicle_follow`, `alerts`,
`stops_search`, `stops_nearby`, `routes`, `update`.

`vehicle_follow` mirrors the `odino vehicle` CLI command, including the
optional `to_stop_id` argument that produces a `destination` summary with
ETA, minutes remaining and stops to go.

### Claude Desktop

Edit `~/Library/Application Support/Claude/claude_desktop_config.json`
(macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "odino": {
      "command": "/Users/<you>/go/bin/odino",
      "args": ["mcp"]
    }
  }
}
```

Restart Claude Desktop after saving.

### Claude Code

```sh
claude mcp add odino "$(which odino)" mcp \
  --env ODINO_LOG_FILE=$HOME/.cache/odino/mcp.log
```

Or edit `~/.claude.json` (or the project's `.mcp.json`) by hand. The
`env.ODINO_LOG_FILE` entry is optional but recommended — Claude Code
swallows the server's `stderr`, so this is the only way to see odino's
own log lines (see [Logs and debugging](#logs-and-debugging)):

```json
{
  "mcpServers": {
    "odino": {
      "command": "/Users/<you>/gitwork/gitlab/_valentino.lauciani/odino/odino",
      "args": ["mcp"],
      "env": { "ODINO_LOG_FILE": "/Users/<you>/.cache/odino/mcp.log" }
    }
  }
}
```

### Codex CLI

Edit `~/.codex/config.toml`:

```toml
[mcp_servers.odino]
command = "/Users/<you>/go/bin/odino"
args = ["mcp"]
```

### MCP via Docker

You can also run the MCP server inside the container — the host's
`~/.cache/odino` is still mounted, so cache state is shared with the
native CLI. The agent config invokes Docker with stdio attached
(`-i`, never `-t`, so the MCP framing isn't corrupted):

```json
{
  "mcpServers": {
    "odino": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "--user", "1000:1000",
        "-v", "/Users/<you>/.cache/odino:/var/cache/odino",
        "odino:latest", "mcp"
      ]
    }
  }
}
```

(Replace `1000:1000` with `$(id -u):$(id -g)` if your MCP host expands it,
otherwise hard-code your own UID/GID.)

After restarting the agent, the model will use `arrivals`, `vehicles`,
`stops_nearby`, etc. as typed tools instead of shelling out.

### Logs and debugging

The MCP protocol owns `stdout` (JSON-RPC framing) and `stdin`, so every
log line goes to `stderr`. Each tool call is logged with name,
arguments and duration:

```
[odino-mcp] 2026/05/23 23:37:04.255913 starting MCP server (version=dev, pid=80096)
[odino-mcp] 2026/05/23 23:37:04.256740 tool=stops_search args={"limit":3,"query":"Odescalchi"}
[odino-mcp] 2026/05/23 23:37:04.263410 tool=stops_search ok in 6.663541ms (response 348 bytes)
```

Where this output lands depends on the host — and most hosts split the
information between two files.

#### Claude Code CLI

Two distinct log destinations, **both useful**:

| What | Where |
| --- | --- |
| Claude-side protocol log (connect, capabilities, per-tool completion timing) | `~/Library/Caches/claude-cli-nodejs/<project-slug>/mcp-logs-odino/<timestamp>.jsonl` |
| Server-side log (the `[odino-mcp] …` lines: args, payload size, internal errors) | the file pointed to by `ODINO_LOG_FILE` |

`<project-slug>` is the working directory you launched `claude` from,
with `/` replaced by `-` (folders starting with `_` become `--`). For
this repo, that's:

```sh
ls ~/Library/Caches/claude-cli-nodejs/-Users-<you>-gitwork-gitlab--valentino-lauciani-odino/mcp-logs-odino/
```

**Claude Code does not capture the MCP server's `stderr`**, so the
detailed `[odino-mcp]` lines are visible only if `ODINO_LOG_FILE` is
set in the MCP config:

```json
{
  "mcpServers": {
    "odino": {
      "command": "/Users/<you>/gitwork/gitlab/_valentino.lauciani/odino/odino",
      "args": ["mcp"],
      "env": { "ODINO_LOG_FILE": "/Users/<you>/.cache/odino/mcp.log" }
    }
  }
}
```

Follow both at once during debugging:

```sh
# terminal 1 — Claude-side protocol & tool timings
tail -F ~/Library/Caches/claude-cli-nodejs/-Users-<you>-*/mcp-logs-odino/*.jsonl

# terminal 2 — server-side: args, response size, internal warnings
tail -F ~/.cache/odino/mcp.log
```

#### Other hosts

| Host | Stderr location |
| --- | --- |
| Claude Desktop (macOS) | `~/Library/Logs/Claude/mcp-server-odino.log` (plus the general `~/Library/Logs/Claude/mcp.log`) |
| Claude Desktop (Windows) | `%APPDATA%\Claude\logs\mcp-server-odino.log` |
| Codex CLI | captured as part of the Codex session log |

These hosts forward the server's `stderr` to disk, so the
`[odino-mcp] …` lines appear without any extra configuration. Setting
`ODINO_LOG_FILE` is still useful as a portable, host-agnostic log you
can `tail -f` directly, and is required when running the MCP server
inside Docker (where the host typically discards container stderr).

## Notes

- Times are reported in `Europe/Rome`.
- Service alert text appears in the language Roma Mobilità publishes
  (mostly Italian), with English used when a translation is available.
- Realtime coverage is provided by Roma Mobilità for ATAC and Roma TPL
  operators. Other operators (Troiani, SAP, BIS, TUSCIA) appear from the
  static schedule only and rows are tagged `SCHED`.
- Roma Mobilità describes the feeds as *experimental*; predictions
  depend on AVM (satellite vehicle monitoring) and may be missing for
  unmonitored runs.

## License

MIT.
