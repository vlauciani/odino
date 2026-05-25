package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/realtime"
)

// mcpLog is the logger used by the MCP server. By default it writes to stderr (the
// host process captures it). If ODINO_LOG_FILE is set, stderr is tee'd into that file.
var mcpLog = log.New(os.Stderr, "[odino-mcp] ", log.LstdFlags|log.Lmicroseconds)

// initMCPLog wires the log destination from ODINO_LOG_FILE (if set).
func initMCPLog() {
	path := strings.TrimSpace(os.Getenv("ODINO_LOG_FILE"))
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		mcpLog.Printf("ODINO_LOG_FILE=%q open failed: %v (continuing with stderr only)", path, err)
		return
	}
	mcpLog.SetOutput(io.MultiWriter(os.Stderr, f))
	mcpLog.Printf("logging to %s", path)
}

// newMCPCmd registers `odino mcp`, an MCP server over stdio that exposes the same
// data the CLI does, so agents can answer queries like "next 64 at Termini".
func newMCPCmd(_ *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server (stdio) exposing odino as agent tools.",
		Long: `Starts a Model Context Protocol server on stdio. Each odino subcommand is
exposed as an MCP tool with JSON output, so an LLM agent can query Rome
bus arrivals, vehicle positions, alerts, stops, and routes directly.

Add to claude_desktop_config.json:

  "odino": { "command": "odino", "args": ["mcp"] }`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPServer(cmd.Context())
		},
	}
}

func runMCPServer(ctx context.Context) error {
	initMCPLog()
	mcpLog.Printf("starting MCP server (version=%s, pid=%d)", Version, os.Getpid())
	srv := mcpserver.NewMCPServer(
		"odino",
		Version,
		mcpserver.WithToolCapabilities(false),
	)

	// arrivals
	srv.AddTool(
		mcp.NewTool("arrivals",
			mcp.WithDescription("Next arrivals at a Rome public-transport stop. Returns live + scheduled passages within a time window, tagged LIVE or SCHED. The stop may be a numeric stop_id or a substring of the stop name. Optionally filter by line short_name."),
			mcp.WithString("stop", mcp.Required(), mcp.Description("Numeric stop_id (e.g. \"70910\") or substring of the stop name (e.g. \"Termini\").")),
			mcp.WithString("route", mcp.Description("Optional route_short_name filter (e.g. \"64\").")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of arrivals to return."), mcp.DefaultNumber(10)),
			mcp.WithNumber("window", mcp.Description("Look-ahead window in minutes."), mcp.DefaultNumber(60)),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			stop := stringArg(args, "stop")
			if stop == "" {
				return nil, fmt.Errorf("stop is required")
			}
			route := stringArg(args, "route")
			limit := intArg(args, "limit", 10)
			window := intArg(args, "window", 60)
			return runArrivalsForMCP(ctx, stop, route, limit, window)
		}),
	)

	// vehicles
	srv.AddTool(
		mcp.NewTool("vehicles",
			mcp.WithDescription("Live vehicle positions, optionally filtered by line short_name. Returns lat/lon, bearing, next stop and delay (when known)."),
			mcp.WithString("route", mcp.Description("Optional route_short_name filter (e.g. \"64\").")),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			route := stringArg(args, "route")
			return runVehiclesForMCP(ctx, route)
		}),
	)

	// alerts
	srv.AddTool(
		mcp.NewTool("alerts",
			mcp.WithDescription("Currently active GTFS-Realtime service alerts (closures, deviations, strikes). Text in English where available."),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			return runAlertsForMCP(ctx)
		}),
	)

	// stops_search
	srv.AddTool(
		mcp.NewTool("stops_search",
			mcp.WithDescription("Search stops by substring of stop_name (case-insensitive)."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Substring of the stop name (e.g. \"Termini\").")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of results."), mcp.DefaultNumber(50)),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			q := stringArg(args, "query")
			if q == "" {
				return nil, fmt.Errorf("query is required")
			}
			limit := intArg(args, "limit", 50)
			return runStopsSearchForMCP(ctx, q, limit)
		}),
	)

	// stops_nearby
	srv.AddTool(
		mcp.NewTool("stops_nearby",
			mcp.WithDescription("List stops within a radius (in metres) of a (lat, lon) coordinate, sorted by ascending distance. Useful for answering 'what stops are near this address?' after geocoding the address externally. Optionally filter by line short_name."),
			mcp.WithNumber("lat", mcp.Required(), mcp.Description("Latitude in decimal degrees (WGS84).")),
			mcp.WithNumber("lon", mcp.Required(), mcp.Description("Longitude in decimal degrees (WGS84).")),
			mcp.WithNumber("radius", mcp.Required(), mcp.Description("Search radius in metres.")),
			mcp.WithString("route", mcp.Description("Optional route_short_name filter (e.g. \"64\").")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of results."), mcp.DefaultNumber(10)),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			lat := floatArg(args, "lat", 0)
			lon := floatArg(args, "lon", 0)
			radius := intArg(args, "radius", 0)
			if radius <= 0 {
				return nil, fmt.Errorf("radius is required and must be > 0 (metres)")
			}
			route := stringArg(args, "route")
			limit := intArg(args, "limit", 10)
			return runStopsNearbyForMCP(ctx, lat, lon, radius, route, limit)
		}),
	)

	// routes
	srv.AddTool(
		mcp.NewTool("routes",
			mcp.WithDescription("List every route (bus/tram/metro line) known in the local cache."),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			return runRoutesForMCP(ctx)
		}),
	)

	// update
	srv.AddTool(
		mcp.NewTool("update",
			mcp.WithDescription("Force a refresh of the GTFS static cache from Roma Mobilità. Returns cache metadata after the refresh."),
		),
		toolHandler(func(ctx context.Context, args map[string]any) (any, error) {
			return runUpdateForMCP(ctx)
		}),
	)

	return mcpserver.ServeStdio(srv)
}

// toolHandler adapts a typed handler returning any+error into the mcp-go ToolHandlerFunc shape.
func toolHandler(fn func(context.Context, map[string]any) (any, error)) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		toolName := req.Params.Name
		args := req.GetArguments()
		start := time.Now()
		mcpLog.Printf("tool=%s args=%s", toolName, summarizeArgs(args))

		v, err := fn(ctx, args)
		took := time.Since(start)
		if err != nil {
			mcpLog.Printf("tool=%s ERROR after %s: %v", toolName, took, err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		buf, err := json.Marshal(v)
		if err != nil {
			mcpLog.Printf("tool=%s marshal ERROR after %s: %v", toolName, took, err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		mcpLog.Printf("tool=%s ok in %s (response %d bytes)", toolName, took, len(buf))
		return mcp.NewToolResultText(string(buf)), nil
	}
}

// summarizeArgs renders the args map compactly for logging; truncates oversized values.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "<unmarshalable>"
	}
	const maxLen = 240
	if len(b) > maxLen {
		return string(b[:maxLen]) + "…"
	}
	return string(b)
}

func stringArg(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return fmt.Sprintf("%v", v)
}

func floatArg(args map[string]any, key string, def float64) float64 {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		var n float64
		_, err := fmt.Sscanf(t, "%f", &n)
		if err != nil {
			return def
		}
		return n
	}
	return def
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		var n int
		_, err := fmt.Sscanf(t, "%d", &n)
		if err != nil {
			return def
		}
		return n
	}
	return def
}

// --- MCP handlers (call into the same core logic as the CLI, but always return JSON-friendly data) ---

// mcpAppForce=true matches the CLI's update behaviour; default false elsewhere.
func mcpApp(ctx context.Context, force bool) (*app, error) {
	flags := &rootFlags{asJSON: true}
	// MCP responses are JSON, so we discard human stdout. Stderr-like messages
	// (cache refresh notices, fetch warnings) go through mcpLog so the host can
	// capture them and ODINO_LOG_FILE picks them up.
	return buildApp(ctx, flags, force, devNull{}, &logWriter{lg: mcpLog})
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// logWriter funnels writes through a *log.Logger, one line per Write (already-line-buffered
// by the GTFS cache progress messages).
type logWriter struct{ lg *log.Logger }

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			w.lg.Print(s)
		}
	}
	return len(p), nil
}

func runArrivalsForMCP(ctx context.Context, stop, route string, limit, window int) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()

	stopID, err := resolveStopForMCP(ctx, a, stop)
	if err != nil {
		return nil, err
	}
	stopName, _ := a.store.StopNameByID(ctx, stopID)
	now := time.Now().In(a.loc)
	windowEnd := now.Add(time.Duration(window) * time.Minute)

	var rtLookup *realtime.TripUpdateLookup
	if rtTU, rtErr := a.rt.TripUpdates(ctx); rtErr == nil {
		rtLookup = realtime.BuildTripUpdateLookup(rtTU)
	}
	var vehByTrip map[string]string
	if vp, err := a.rt.VehiclePositions(ctx); err == nil {
		vehByTrip = realtime.VehicleByTrip(vp)
	}
	scheduled, err := scheduledInWindow(ctx, a.store, stopID, route, now, windowEnd)
	if err != nil {
		return nil, err
	}
	rows := mergeArrivals(scheduled, rtLookup, vehByTrip, stopID, now, windowEnd, route, a, ctx)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	var alerts []AlertView
	if msg, err := a.rt.ServiceAlerts(ctx); err == nil {
		alerts = filterRelevantAlerts(collectAlerts(msg, a.store, now, ctx), stopID, rowsRouteShorts(rows), route)
	}
	return map[string]any{
		"stop":     map[string]string{"stop_id": stopID, "stop_name": stopName},
		"arrivals": rows,
		"alerts":   alerts,
		"now":      now.Format(time.RFC3339),
	}, nil
}

// resolveStopForMCP is the MCP-side resolver — it never prints a disambiguation table,
// instead returning an error listing candidates so the agent can ask back.
func resolveStopForMCP(ctx context.Context, a *app, query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("empty stop query")
	}
	if isNumeric(q) {
		s, err := a.store.StopByID(ctx, q)
		if err != nil {
			return "", err
		}
		if s == nil {
			return "", fmt.Errorf("no stop with id %q", q)
		}
		return s.StopID, nil
	}
	stops, err := a.store.SearchStopsByName(ctx, q, 20)
	if err != nil {
		return "", err
	}
	switch len(stops) {
	case 0:
		return "", fmt.Errorf("no stop matches %q", q)
	case 1:
		return stops[0].StopID, nil
	}
	candidates := make([]string, 0, len(stops))
	for _, s := range stops {
		candidates = append(candidates, fmt.Sprintf("%s (%s)", s.StopName, s.StopID))
	}
	return "", fmt.Errorf("multiple stops match %q: %s — call with a specific stop_id", q, strings.Join(candidates, "; "))
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func runVehiclesForMCP(ctx context.Context, routeShort string) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	var routeIDSet map[string]struct{}
	if routeShort != "" {
		ids, err := a.store.RouteIDsByShortName(ctx, routeShort)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("no route with short_name %q", routeShort)
		}
		routeIDSet = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			routeIDSet[id] = struct{}{}
		}
	}
	msg, err := a.rt.VehiclePositions(ctx)
	if err != nil {
		return nil, err
	}
	type vehicleRow struct {
		VehicleID string   `json:"vehicle_id"`
		RouteID   string   `json:"route_id"`
		RouteName string   `json:"route_short_name"`
		TripID    string   `json:"trip_id"`
		Lat       float32  `json:"lat"`
		Lon       float32  `json:"lon"`
		Bearing   *float32 `json:"bearing"` // null when the feed doesn't report it
		NextStop  string   `json:"next_stop"`
		Updated   string   `json:"updated_at"`
	}
	var rows []vehicleRow
	for _, ent := range msg.GetEntity() {
		vp := ent.GetVehicle()
		if vp == nil {
			continue
		}
		rid := vp.GetTrip().GetRouteId()
		if routeIDSet != nil {
			if _, ok := routeIDSet[rid]; !ok {
				continue
			}
		}
		pos := vp.GetPosition()
		r := vehicleRow{
			VehicleID: vp.GetVehicle().GetId(),
			RouteID:   rid,
			TripID:    vp.GetTrip().GetTripId(),
			Lat:       pos.GetLatitude(),
			Lon:       pos.GetLongitude(),
			NextStop:  vp.GetStopId(),
		}
		if pos != nil && pos.Bearing != nil {
			b := *pos.Bearing
			r.Bearing = &b
		}
		if vp.GetTimestamp() > 0 {
			r.Updated = time.Unix(int64(vp.GetTimestamp()), 0).In(a.loc).Format(time.RFC3339)
		}
		if rn, err := lookupRouteShort(ctx, a.store, rid); err == nil {
			r.RouteName = rn
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func runAlertsForMCP(ctx context.Context) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	msg, err := a.rt.ServiceAlerts(ctx)
	if err != nil {
		return nil, err
	}
	return collectAlerts(msg, a.store, time.Now().In(a.loc), ctx), nil
}

func runStopsNearbyForMCP(ctx context.Context, lat, lon float64, radius int, route string, limit int) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	return a.store.StopsNearby(ctx, lat, lon, radius, route, limit)
}

func runStopsSearchForMCP(ctx context.Context, query string, limit int) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	return a.store.SearchStopsByName(ctx, query, limit)
}

func runRoutesForMCP(ctx context.Context) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	return a.store.ListRoutes(ctx)
}

func runUpdateForMCP(ctx context.Context) (any, error) {
	a, err := mcpApp(ctx, true)
	if err != nil {
		return nil, err
	}
	defer a.close()
	last, _ := a.store.LastUpdated()
	return map[string]any{
		"cache_dir":    a.cache.Dir,
		"sqlite_path":  a.cache.DBPath(),
		"last_updated": last.Format(time.RFC3339),
	}, nil
}
