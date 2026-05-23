package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"

	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/output"
	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/store"
)

// --- update ---

func newUpdateCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Force a refresh of the GTFS static cache.",
		Long:  "Downloads the latest rome_static_gtfs.zip from Roma Mobilità, validates its MD5, and rebuilds the local SQLite cache. The cache is also refreshed lazily by other commands when older than 24 hours.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, true, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			last, _ := a.store.LastUpdated()
			fmt.Fprintf(out, "Cache directory: %s\nSQLite database:   %s\nLast updated:     %s\n",
				a.cache.Dir, a.cache.DBPath(), last.In(a.loc).Format(time.RFC3339))
			return nil
		},
	}
}

// --- stops search ---

func newStopsCmd(flags *rootFlags) *cobra.Command {
	c := &cobra.Command{
		Use:   "stops",
		Short: "Look up stops in the local GTFS cache.",
	}
	c.AddCommand(newStopsSearchCmd(flags), newStopsNearbyCmd(flags))
	return c
}

func newStopsNearbyCmd(flags *rootFlags) *cobra.Command {
	var (
		lat, lon   float64
		radius     int
		limit      int
		routeShort string
	)
	cmd := &cobra.Command{
		Use:   "nearby",
		Short: "List stops within a radius (in metres) of a coordinate.",
		Long: `Return stops whose great-circle distance from (--lat, --lon) is less than
--radius metres, sorted by ascending distance. Use --route to keep only stops
served by a specific line (e.g. --route 64).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			if radius <= 0 {
				return fmt.Errorf("--radius is required and must be > 0 (metres)")
			}
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			stops, err := a.store.StopsNearby(cmd.Context(), lat, lon, radius, routeShort, limit)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return output.JSON(out, stops)
			}
			if len(stops) == 0 {
				fmt.Fprintln(out, "(no stops in range)")
				return nil
			}
			rows := make([][]string, 0, len(stops))
			for _, s := range stops {
				rows = append(rows, []string{
					s.StopID, s.StopName,
					fmtFloat(s.StopLat), fmtFloat(s.StopLon),
					strconv.Itoa(s.DistanceM),
				})
			}
			return output.Table(out, []string{"stop_id", "name", "lat", "lon", "distance_m"}, rows)
		},
	}
	cmd.Flags().Float64Var(&lat, "lat", 0, "Latitude (WGS84, decimal degrees). Required.")
	cmd.Flags().Float64Var(&lon, "lon", 0, "Longitude (WGS84, decimal degrees). Required.")
	cmd.Flags().IntVar(&radius, "radius", 0, "Search radius in metres. Required.")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum number of results (0 = no limit).")
	cmd.Flags().StringVar(&routeShort, "route", "", "Filter to stops served by this route_short_name (e.g. 64).")
	_ = cmd.MarkFlagRequired("lat")
	_ = cmd.MarkFlagRequired("lon")
	_ = cmd.MarkFlagRequired("radius")
	return cmd
}

func newStopsSearchCmd(flags *rootFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search stops by name (substring, case-insensitive).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			stops, err := a.store.SearchStopsByName(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return output.JSON(out, stops)
			}
			rows := make([][]string, 0, len(stops))
			for _, s := range stops {
				rows = append(rows, []string{s.StopID, s.StopName, fmtFloat(s.StopLat), fmtFloat(s.StopLon)})
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "(no matching stops)")
				return nil
			}
			return output.Table(out, []string{"stop_id", "name", "lat", "lon"}, rows)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of results (0 = no limit).")
	return cmd
}

// --- routes ---

func newRoutesCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "routes",
		Short: "List all routes (bus/tram/metro lines) in the cache.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			routes, err := a.store.ListRoutes(cmd.Context())
			if err != nil {
				return err
			}
			if flags.asJSON {
				return output.JSON(out, routes)
			}
			rows := make([][]string, 0, len(routes))
			seenShort := map[string]bool{}
			for _, r := range routes {
				key := r.RouteShortName + "|" + r.RouteLongName
				if seenShort[key] {
					continue
				}
				seenShort[key] = true
				rows = append(rows, []string{r.RouteShortName, r.RouteLongName, routeTypeName(r.RouteType), r.AgencyID})
			}
			return output.Table(out, []string{"short_name", "long_name", "type", "agency"}, rows)
		},
	}
}

func routeTypeName(t int) string {
	// GTFS route_type basic categories.
	switch t {
	case 0:
		return "tram"
	case 1:
		return "metro"
	case 2:
		return "rail"
	case 3:
		return "bus"
	case 4:
		return "ferry"
	case 5:
		return "cable"
	case 6:
		return "gondola"
	case 7:
		return "funicular"
	case 11:
		return "trolleybus"
	case 12:
		return "monorail"
	default:
		return fmt.Sprintf("type-%d", t)
	}
}

// --- vehicles ---

func newVehiclesCmd(flags *rootFlags) *cobra.Command {
	var routeShort string
	cmd := &cobra.Command{
		Use:   "vehicles",
		Short: "Show live vehicle positions, optionally filtered by route.",
		Long:  "Fetches the GTFS-RT vehicle_positions feed and prints one row per active vehicle. Use --route to filter by line (route_short_name).",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()

			// Resolve route_id set for the requested short_name (if any).
			var routeIDSet map[string]struct{}
			if routeShort != "" {
				ids, err := a.store.RouteIDsByShortName(cmd.Context(), routeShort)
				if err != nil {
					return err
				}
				if len(ids) == 0 {
					return fmt.Errorf("no route with short_name %q in the cache (try `odino routes`)", routeShort)
				}
				routeIDSet = make(map[string]struct{}, len(ids))
				for _, id := range ids {
					routeIDSet[id] = struct{}{}
				}
			}

			msg, err := a.rt.VehiclePositions(cmd.Context())
			if err != nil {
				return fmt.Errorf("fetching vehicle positions: %w", err)
			}

			// Build trip→delay lookup from trip_updates for delay column (best-effort).
			tu, _ := a.rt.TripUpdates(cmd.Context())
			delays := map[string]int32{}
			if tu != nil {
				for _, ent := range tu.GetEntity() {
					t := ent.GetTripUpdate()
					if t == nil || t.GetTrip().GetTripId() == "" {
						continue
					}
					if t.Delay != nil {
						delays[t.GetTrip().GetTripId()] = t.GetDelay()
					}
				}
			}

			type vehicleRow struct {
				VehicleID string  `json:"vehicle_id"`
				RouteID   string  `json:"route_id"`
				RouteName string  `json:"route_short_name"`
				TripID    string  `json:"trip_id"`
				Lat       float32 `json:"lat"`
				Lon       float32 `json:"lon"`
				Bearing   float32 `json:"bearing"`
				NextStop  string  `json:"next_stop"`
				DelayMin  *int    `json:"delay_min,omitempty"`
				Updated   string  `json:"updated_at"`
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
				tripID := vp.GetTrip().GetTripId()
				row := vehicleRow{
					VehicleID: vp.GetVehicle().GetId(),
					RouteID:   rid,
					TripID:    tripID,
					Lat:       pos.GetLatitude(),
					Lon:       pos.GetLongitude(),
					Bearing:   pos.GetBearing(),
					NextStop:  vp.GetStopId(),
				}
				if vp.GetTimestamp() > 0 {
					row.Updated = time.Unix(int64(vp.GetTimestamp()), 0).In(a.loc).Format("15:04:05")
				}
				// Resolve route_short_name for display.
				if rid != "" {
					if r, err := lookupRouteShort(cmd.Context(), a.store, rid); err == nil {
						row.RouteName = r
					}
				}
				// Resolve next_stop name if available.
				if row.NextStop != "" {
					if name, err := a.store.StopNameByID(cmd.Context(), row.NextStop); err == nil && name != "" {
						row.NextStop = fmt.Sprintf("%s (%s)", row.NextStop, name)
					}
				}
				if d, ok := delays[tripID]; ok {
					m := int(d / 60)
					row.DelayMin = &m
				}
				rows = append(rows, row)
			}

			if flags.asJSON {
				return output.JSON(out, rows)
			}
			if len(rows) == 0 {
				if routeShort != "" {
					fmt.Fprintf(out, "(no live vehicles for line %s)\n", routeShort)
				} else {
					fmt.Fprintln(out, "(no live vehicles)")
				}
				return nil
			}
			tableRows := make([][]string, 0, len(rows))
			for _, r := range rows {
				delay := ""
				if r.DelayMin != nil {
					delay = fmt.Sprintf("%+d", *r.DelayMin)
				}
				tableRows = append(tableRows, []string{
					r.VehicleID, r.RouteName, fmtFloat(float64(r.Lat)), fmtFloat(float64(r.Lon)),
					fmt.Sprintf("%.0f", r.Bearing), r.NextStop, delay, r.Updated,
				})
			}
			return output.Table(out, []string{"vehicle", "line", "lat", "lon", "bearing", "next_stop", "delay_min", "updated"}, tableRows)
		},
	}
	cmd.Flags().StringVar(&routeShort, "route", "", "Filter by route_short_name (e.g. 64).")
	return cmd
}

// --- alerts ---

func newAlertsCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "alerts",
		Short: "List active service alerts.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			msg, err := a.rt.ServiceAlerts(cmd.Context())
			if err != nil {
				return fmt.Errorf("fetching service alerts: %w", err)
			}
			alerts := collectAlerts(msg, a.store, time.Now().In(a.loc), cmd.Context())
			if flags.asJSON {
				return output.JSON(out, alerts)
			}
			if len(alerts) == 0 {
				fmt.Fprintln(out, "(no active alerts)")
				return nil
			}
			rows := make([][]string, 0, len(alerts))
			for _, al := range alerts {
				rows = append(rows, []string{
					al.ID,
					al.Severity,
					truncate(al.Header, 80),
					strings.Join(al.AffectedRoutes, ","),
					formatTime(al.Start, a.loc),
					formatTime(al.End, a.loc),
				})
			}
			return output.Table(out, []string{"id", "severity", "header", "routes", "start", "end"}, rows)
		},
	}
}

// AlertView is the rendered shape for an alert.
type AlertView struct {
	ID             string    `json:"id"`
	Severity       string    `json:"severity"`
	Cause          string    `json:"cause"`
	Effect         string    `json:"effect"`
	Header         string    `json:"header"`
	Description    string    `json:"description"`
	AffectedRoutes []string  `json:"affected_routes,omitempty"`
	AffectedStops  []string  `json:"affected_stops,omitempty"`
	Start          time.Time `json:"start,omitempty"`
	End            time.Time `json:"end,omitempty"`
}

// collectAlerts maps a service_alerts FeedMessage to a slice of AlertView, keeping only
// alerts whose ActivePeriod includes now (or has no period, treated as always-on).
func collectAlerts(msg *gtfsrt.FeedMessage, st *store.Store, now time.Time, ctx interface{}) []AlertView {
	_ = ctx
	if msg == nil {
		return nil
	}
	out := []AlertView{}
	nowSec := now.Unix()
	for _, ent := range msg.GetEntity() {
		al := ent.GetAlert()
		if al == nil {
			continue
		}
		// active period filter
		active := true
		if len(al.GetActivePeriod()) > 0 {
			active = false
			for _, p := range al.GetActivePeriod() {
				s := int64(p.GetStart())
				e := int64(p.GetEnd())
				if (s == 0 || nowSec >= s) && (e == 0 || nowSec <= e) {
					active = true
					break
				}
			}
		}
		if !active {
			continue
		}
		v := AlertView{
			ID:          ent.GetId(),
			Severity:    severityName(al.SeverityLevel),
			Cause:       causeName(al.Cause),
			Effect:      effectName(al.Effect),
			Header:      pickTranslation(al.GetHeaderText()),
			Description: pickTranslation(al.GetDescriptionText()),
		}
		if len(al.GetActivePeriod()) > 0 {
			p := al.GetActivePeriod()[0]
			if p.GetStart() > 0 {
				v.Start = time.Unix(int64(p.GetStart()), 0)
			}
			if p.GetEnd() > 0 {
				v.End = time.Unix(int64(p.GetEnd()), 0)
			}
		}
		routeSet := map[string]struct{}{}
		stopSet := map[string]struct{}{}
		for _, ie := range al.GetInformedEntity() {
			if rid := ie.GetRouteId(); rid != "" {
				if r, err := lookupRouteShort(nil, st, rid); err == nil && r != "" {
					routeSet[r] = struct{}{}
				} else {
					routeSet[rid] = struct{}{}
				}
			}
			if sid := ie.GetStopId(); sid != "" {
				stopSet[sid] = struct{}{}
			}
		}
		v.AffectedRoutes = setKeys(routeSet)
		v.AffectedStops = setKeys(stopSet)
		out = append(out, v)
	}
	return out
}

// alertsByRoute groups alerts by route_short_name for quick lookup.
func alertsByRoute(alerts []AlertView) map[string][]AlertView {
	out := map[string][]AlertView{}
	for _, a := range alerts {
		for _, r := range a.AffectedRoutes {
			out[r] = append(out[r], a)
		}
	}
	return out
}

// alertsByStop groups alerts by stop_id for quick lookup.
func alertsByStop(alerts []AlertView) map[string][]AlertView {
	out := map[string][]AlertView{}
	for _, a := range alerts {
		for _, s := range a.AffectedStops {
			out[s] = append(out[s], a)
		}
	}
	return out
}

func pickTranslation(ts *gtfsrt.TranslatedString) string {
	if ts == nil {
		return ""
	}
	pref := []string{"en", "it"} // EN preferred per spec; IT fallback
	for _, lang := range pref {
		for _, tr := range ts.GetTranslation() {
			if strings.EqualFold(tr.GetLanguage(), lang) {
				return strings.TrimSpace(tr.GetText())
			}
		}
	}
	if len(ts.GetTranslation()) > 0 {
		return strings.TrimSpace(ts.GetTranslation()[0].GetText())
	}
	return ""
}

func severityName(s *gtfsrt.Alert_SeverityLevel) string {
	if s == nil {
		return "UNKNOWN"
	}
	return strings.ToUpper(s.String())
}

func causeName(c *gtfsrt.Alert_Cause) string {
	if c == nil {
		return ""
	}
	return strings.ToUpper(c.String())
}

func effectName(e *gtfsrt.Alert_Effect) string {
	if e == nil {
		return ""
	}
	return strings.ToUpper(e.String())
}

func setKeys(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

// --- helpers shared across commands ---

func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func formatTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// lookupRouteShort returns the route_short_name for a route_id. If ctx is nil a background
// context is used; this lets `alerts` (which has no command context handy) reuse the helper.
func lookupRouteShort(ctx interface{}, st *store.Store, routeID string) (string, error) {
	// We can't pass nil directly to QueryRowContext; use a TODO context when ctx is nil.
	type ctxLike interface{ Done() <-chan struct{} }
	_ = ctxLike(nil)
	if routeID == "" {
		return "", nil
	}
	var v string
	err := st.DB().QueryRow(`SELECT IFNULL(route_short_name, route_id) FROM routes WHERE route_id=?`, routeID).Scan(&v)
	return v, err
}
