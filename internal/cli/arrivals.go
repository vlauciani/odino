package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vlauciani/odino/internal/output"
	"github.com/vlauciani/odino/internal/realtime"
	"github.com/vlauciani/odino/internal/store"
)

// arrivalRow is one rendered row in the arrivals view.
type arrivalRow struct {
	Time      time.Time `json:"time"`
	Line      string    `json:"line"`
	Vehicle   string    `json:"vehicle,omitempty"` // empty for SCHED rows; populated for LIVE when known
	Headsign  string    `json:"headsign"`
	Min       int       `json:"minutes"`
	Source    string    `json:"source"` // LIVE | SCHED
	TripID    string    `json:"trip_id"`
	RouteID   string    `json:"route_id"`
	StopID    string    `json:"stop_id,omitempty"` // set only on aggregated multi-pole arrivals
}

func newArrivalsCmd(flags *rootFlags) *cobra.Command {
	var (
		routeShort string
		limit      int
		window     int
	)
	cmd := &cobra.Command{
		Use:   "arrivals <stop>",
		Short: "Next arrivals at a stop (live + scheduled).",
		Long: `Show the next arrivals at a stop within a time window.

<stop> may be a numeric stop_id (e.g. 70910) or a substring of the stop name
(e.g. "Termini"). If multiple stops match the name, they are listed for you
to disambiguate.

Realtime trip_updates from the GTFS-RT feed are merged with the planned
schedule. Each row is tagged LIVE (delay applied) or SCHED (planned only).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			return runArrivals(cmd.Context(), a, args[0], routeShort, limit, window)
		},
	}
	cmd.Flags().StringVar(&routeShort, "route", "", "Filter by route_short_name (e.g. 64).")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum number of arrivals to show.")
	cmd.Flags().IntVar(&window, "window", 60, "Look-ahead window in minutes.")
	return cmd
}

func runArrivals(ctx context.Context, a *app, query, routeShort string, limit, windowMin int) error {
	// 1. Resolve stop_id.
	stopID, err := resolveStop(ctx, a, query)
	if err != nil {
		return err
	}
	if stopID == "" {
		// Disambiguation message already printed.
		return nil
	}
	stopName, _ := a.store.StopNameByID(ctx, stopID)

	now := time.Now().In(a.loc)
	windowEnd := now.Add(time.Duration(windowMin) * time.Minute)

	// 2. Fetch realtime trip_updates (graceful degrade on failure).
	var rtLookup *realtime.TripUpdateLookup
	tu, rtErr := a.rt.TripUpdates(ctx)
	if rtErr != nil {
		a.printErr("realtime unavailable, falling back to schedule only: %v", rtErr)
	} else {
		rtLookup = realtime.BuildTripUpdateLookup(tu)
	}
	// Build a trip → vehicle fallback map from vehicle_positions, used when a trip_update
	// doesn't carry its own vehicle descriptor. Best-effort: silent on failure.
	var vehByTrip map[string]string
	if vp, err := a.rt.VehiclePositions(ctx); err == nil {
		vehByTrip = realtime.VehicleByTrip(vp)
	}

	// 3. Pull scheduled arrivals from the local cache.
	//    Service day for the search: today and (if window crosses midnight) tomorrow.
	scheduled, err := scheduledInWindow(ctx, a.store, stopID, routeShort, now, windowEnd)
	if err != nil {
		return err
	}

	// 4. Merge planned + RT.
	rows := mergeArrivals(scheduled, rtLookup, vehByTrip, stopID, now, windowEnd, routeShort, a, ctx)

	// 5. Sort by predicted time, apply limit.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time.Before(rows[j].Time) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	// 6. Service alerts for this stop / matching routes (one-line banner).
	var alertViews []AlertView
	if alertsMsg, err := a.rt.ServiceAlerts(ctx); err == nil {
		alertViews = collectAlerts(alertsMsg, a.store, now, ctx)
	}
	relevantAlerts := filterRelevantAlerts(alertViews, stopID, rowsRouteShorts(rows), routeShort)

	if a.flags.asJSON {
		payload := struct {
			Stop     map[string]string `json:"stop"`
			Arrivals []arrivalRow      `json:"arrivals"`
			Alerts   []AlertView       `json:"alerts,omitempty"`
		}{
			Stop:     map[string]string{"stop_id": stopID, "stop_name": stopName},
			Arrivals: rows,
			Alerts:   relevantAlerts,
		}
		return output.JSON(a.out, payload)
	}

	// Header line.
	headerLine := fmt.Sprintf("Stop: %s (%s) — %s", stopName, stopID, now.Format("Mon 02 Jan 15:04"))
	fmt.Fprintln(a.out, headerLine)
	for _, al := range relevantAlerts {
		msg := strings.TrimSpace(al.Header)
		if msg == "" {
			msg = al.Effect
		}
		if len(al.AffectedRoutes) > 0 {
			msg = fmt.Sprintf("Lines %s: %s", strings.Join(al.AffectedRoutes, ","), msg)
		}
		output.Banner(a.out, truncate(msg, 100))
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.out, "(no arrivals in the requested window)")
		return nil
	}
	tableRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		tableRows = append(tableRows, []string{
			r.Time.In(a.loc).Format("15:04"),
			r.Line,
			r.Vehicle, // empty for SCHED rows
			truncate(r.Headsign, 40),
			strconv.Itoa(r.Min),
			r.Source,
		})
	}
	return output.Table(a.out, []string{"time", "line", "vehicle", "headsign", "min", "src"}, tableRows)
}

// resolveStop auto-detects whether query is a numeric stop_id or a substring search.
// Returns the resolved stop_id, or empty string if a disambiguation list has been printed.
func resolveStop(ctx context.Context, a *app, query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("empty stop query")
	}
	// Try numeric stop_id first.
	if _, err := strconv.Atoi(q); err == nil {
		s, err := a.store.StopByID(ctx, q)
		if err != nil {
			return "", err
		}
		if s != nil {
			return s.StopID, nil
		}
		return "", fmt.Errorf("no stop with id %q", q)
	}
	// String search.
	stops, err := a.store.SearchStopsByName(ctx, q, 50)
	if err != nil {
		return "", err
	}
	switch len(stops) {
	case 0:
		return "", fmt.Errorf("no stop matches %q", q)
	case 1:
		return stops[0].StopID, nil
	}
	// Multiple matches → list and ask user to be more specific.
	fmt.Fprintf(a.err, "Multiple stops match %q (%d). Re-run with the stop_id you want:\n\n", q, len(stops))
	rows := make([][]string, 0, len(stops))
	for _, s := range stops {
		rows = append(rows, []string{s.StopID, s.StopName, fmtFloat(s.StopLat), fmtFloat(s.StopLon)})
	}
	_ = output.Table(a.err, []string{"stop_id", "name", "lat", "lon"}, rows)
	return "", nil
}

// scheduledInWindow returns scheduled arrivals at stopID whose predicted time falls
// between now and end. Handles the case where the window crosses midnight by querying
// today's and tomorrow's service days, with GTFS "extended hours" (>=24:00) for today.
func scheduledInWindow(ctx context.Context, st *store.Store, stopID, routeShort string, now, end time.Time) ([]plannedRow, error) {
	out := []plannedRow{}

	// Today's service day.
	todayServices, err := st.ActiveServiceIDs(ctx, now)
	if err != nil {
		return nil, err
	}
	if len(todayServices) > 0 {
		startSec := secOfDay(now)
		endSec := secOfDay(now) + int(end.Sub(now).Seconds())
		rows, err := st.ScheduledArrivalsAt(ctx, stopID, todayServices, startSec, endSec, routeShort, 0)
		if err != nil {
			return nil, err
		}
		// Anchor each row to the actual clock time on `now`'s date (handles 25:00 → 01:00 next day).
		base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		for _, r := range rows {
			out = append(out, plannedRow{ScheduledArrival: r, predicted: base.Add(time.Duration(r.DepartureTime) * time.Second)})
		}
	}

	// Tomorrow's service day (if window crosses midnight clock-wise AND tomorrow's services exist).
	if end.Day() != now.Day() {
		tomorrow := now.Add(24 * time.Hour)
		tomServices, err := st.ActiveServiceIDs(ctx, tomorrow)
		if err != nil {
			return nil, err
		}
		if len(tomServices) > 0 {
			// Tomorrow's clock window is [00:00, end.SecOfDay()).
			tomStart := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, now.Location())
			endSec := secOfDay(end)
			rows, err := st.ScheduledArrivalsAt(ctx, stopID, tomServices, 0, endSec, routeShort, 0)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				out = append(out, plannedRow{ScheduledArrival: r, predicted: tomStart.Add(time.Duration(r.DepartureTime) * time.Second)})
			}
		}
	}
	return out, nil
}

type plannedRow struct {
	store.ScheduledArrival
	predicted time.Time
}

// mergeArrivals overlays realtime trip_updates onto the planned rows, and appends any
// realtime-only updates whose trip is not in the planned set (e.g. added trips).
// stopID is the target stop_id every planned row was filtered to.
// vehByTrip is a fallback map from trip_id to vehicle_id when trip_updates omit the
// vehicle descriptor.
func mergeArrivals(planned []plannedRow, rt *realtime.TripUpdateLookup, vehByTrip map[string]string, stopID string, now, end time.Time, routeShort string, a *app, ctx context.Context) []arrivalRow {
	seenTripStop := map[string]bool{}
	out := make([]arrivalRow, 0, len(planned))

	for _, p := range planned {
		seenTripStop[p.TripID+"|"+stopID] = true
		row := arrivalRow{
			Time:     p.predicted,
			Line:     p.RouteShortName,
			Headsign: p.TripHeadsign,
			Source:   "SCHED",
			TripID:   p.TripID,
			RouteID:  p.RouteID,
		}
		if rt != nil {
			if ev := rt.Get(p.TripID, stopID); ev != nil {
				if ev.Cancelled {
					continue
				}
				if ev.HasTime {
					row.Time = ev.Time
					row.Source = "LIVE"
				} else if ev.HasDelay {
					row.Time = p.predicted.Add(time.Duration(ev.Delay) * time.Second)
					row.Source = "LIVE"
				}
			} else if d, ok := rt.TripDelay(p.TripID); ok {
				row.Time = p.predicted.Add(time.Duration(d) * time.Second)
				row.Source = "LIVE"
			}
		}
		if row.Source == "LIVE" {
			row.Vehicle = resolveVehicleID(rt, vehByTrip, p.TripID)
		}
		row.Min = int(row.Time.Sub(now).Minutes())
		if row.Time.Before(now.Add(-1 * time.Minute)) {
			// Already departed (with a 1-min grace); skip.
			continue
		}
		if row.Time.After(end) {
			continue
		}
		out = append(out, row)
	}

	// Append realtime-only rows for the target stop that aren't already in the planned set
	// (e.g. added/unscheduled trips).
	if rt != nil {
		for tripID, stopIDs := range rt.TripStops() {
			for _, sid := range stopIDs {
				if sid != stopID {
					continue
				}
				if seenTripStop[tripID+"|"+sid] {
					continue
				}
				ev := rt.Get(tripID, sid)
				if ev == nil || ev.Cancelled {
					continue
				}
				// Only consider events with an absolute time within window.
				if !ev.HasTime {
					continue
				}
				if ev.Time.Before(now) || ev.Time.After(end) {
					continue
				}
				// Resolve route info for display.
				info, err := a.store.TripInfoByID(ctx, tripID)
				if err != nil || info == nil {
					continue
				}
				if routeShort != "" && !strings.EqualFold(info.RouteShortName, routeShort) {
					continue
				}
				out = append(out, arrivalRow{
					Time:     ev.Time,
					Line:     info.RouteShortName,
					Vehicle:  resolveVehicleID(rt, vehByTrip, tripID),
					Headsign: info.TripHeadsign,
					Min:      int(ev.Time.Sub(now).Minutes()),
					Source:   "LIVE",
					TripID:   tripID,
					RouteID:  info.RouteID,
				})
			}
		}
	}

	return out
}

func secOfDay(t time.Time) int {
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

// resolveVehicleID picks a vehicle id for the trip, preferring the trip_update's own
// vehicle descriptor and falling back to the trip→vehicle map built from vehicle_positions.
func resolveVehicleID(rt *realtime.TripUpdateLookup, vehByTrip map[string]string, tripID string) string {
	if v := rt.VehicleID(tripID); v != "" {
		return v
	}
	if vehByTrip != nil {
		return vehByTrip[tripID]
	}
	return ""
}

func filterRelevantAlerts(all []AlertView, stopID string, lineShorts []string, filterShort string) []AlertView {
	if len(all) == 0 {
		return nil
	}
	wantLine := map[string]bool{}
	for _, l := range lineShorts {
		if l == "" {
			continue
		}
		wantLine[strings.ToLower(l)] = true
	}
	if filterShort != "" {
		wantLine[strings.ToLower(filterShort)] = true
	}
	out := []AlertView{}
	seen := map[string]bool{}
	for _, al := range all {
		if seen[al.ID] {
			continue
		}
		match := false
		for _, sid := range al.AffectedStops {
			if sid == stopID {
				match = true
				break
			}
		}
		if !match {
			for _, r := range al.AffectedRoutes {
				if wantLine[strings.ToLower(r)] {
					match = true
					break
				}
			}
		}
		if match {
			out = append(out, al)
			seen[al.ID] = true
		}
	}
	return out
}

func rowsRouteShorts(rows []arrivalRow) []string {
	set := map[string]bool{}
	for _, r := range rows {
		set[r.Line] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
