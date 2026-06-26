# Release Notes

### Release 1.0.0 (2026-06-26)
  - Add Rome public-transport CLI backed by the Roma Mobilità GTFS static + GTFS-Realtime feeds (no API key required)
  - Add `arrivals` command merging live trip updates with the planned schedule, each row tagged LIVE or SCHED
  - Add `vehicles` command for live vehicle positions, optionally filtered by line
  - Add `vehicle` follow command with `--to`/`--limit` (ETA, minutes remaining, stops to go)
  - Add `alerts`, `stops search`, `stops nearby`, and `routes` commands
  - Add an MCP server (`odino mcp`) over stdio exposing every command as a JSON tool for AI agents
  - Make the MCP `arrivals` tool place-aware: resolve a place name to its poles and disambiguate down a line -> direction -> pole funnel, returning a structured `ambiguous` payload instead of an error
  - Enrich `stops_search` and `stops_nearby` with `lines_served` (route_short_name + direction headsign) per pole
  - Include the vehicle id in arrivals from trip updates or vehicle positions
  - Add `ODINO_LOG_FILE` logging with per-tool-call timing for the MCP server
  - Embed the IANA tz database in the binary so `Europe/Rome` resolves on any host
  - Add a GoReleaser release pipeline producing cross-platform binaries and a multi-arch Docker Hub image from a single tag
