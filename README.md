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
git clone https://gitlab.rm.ingv.it/valentino.lauciani/odino.git
cd odino
go build -o odino ./cmd/odino
```

### Via `go install`

```sh
go install gitlab.rm.ingv.it/valentino.lauciani/odino/cmd/odino@latest
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
odino arrivals 79790
odino arrivals "Vittorio Emanuele/Argentina"

# Filter by line.
odino arrivals 79790 --route 64

# JSON output (for scripts and agents).
odino arrivals 79790 --route 64 --json | jq .

# Live position of every line-64 bus.
odino vehicles --route 64

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

# Next arrivals at C.so Vittorio Emanuele / Argentina (stop 79790), line 64.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest arrivals 79790 --route 64

# Stops within 250 m of a coordinate.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest stops nearby --lat 41.853563 --lon 12.499133 --radius 250

# Live vehicle positions for line 64.
docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino \
  odino:latest vehicles --route 64 --json
```

Handy shell alias:

```sh
alias odino='docker run --rm --user $(id -u):$(id -g) \
  -v ~/.cache/odino:/var/cache/odino odino:latest'
# Then: odino arrivals 79790 --route 64
```

## MCP server

`odino mcp` starts a Model Context Protocol server on stdio. Each CLI
subcommand is exposed as an MCP tool returning JSON.

Available tools: `arrivals`, `vehicles`, `alerts`, `stops_search`,
`stops_nearby`, `routes`, `update`.

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
claude mcp add odino "$(which odino)" mcp
```

Or edit `~/.claude/settings.json` (or `.claude/settings.json` in the
project) by hand:

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
