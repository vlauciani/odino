package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/realtime"
	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/store"
)

// A "place" (Termini, Odescalchi) is what a human names; Rome's GTFS has no station, so a
// place is reconstructed as the set of poles whose stop_name shares the query, then narrowed
// down the funnel line → direction → pole. See CONTEXT.md / docs/adr/0001.

// placeStopwords are Italian street-type words and articles dropped before matching a free-text
// place query against stop_name, so "Viale Carlo Tommaso Odescalchi" reduces to its salient
// tokens. GTFS abbreviates street names to a single token, hence the token fallback.
var placeStopwords = map[string]bool{
	"via": true, "viale": true, "vle": true, "piazza": true, "pza": true, "piazzale": true,
	"largo": true, "corso": true, "cso": true, "vicolo": true, "salita": true, "circonvallazione": true,
	"lungotevere": true, "borgo": true, "clivo": true, "galleria": true, "ponte": true,
	"di": true, "del": true, "della": true, "dei": true, "delle": true, "dello": true,
	"da": true, "il": true, "lo": true, "la": true, "gli": true, "le": true, "al": true,
}

// significantStopTokens splits q into the meaningful tokens to match against stop_name.
func significantStopTokens(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 3 || placeStopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// resolvePlace turns a free-text place query into the poles it denotes. It tries the whole
// phrase first (catches exact stop names like "appia antica/caffarella"). On no match it falls
// back to the significant tokens, but which tokens to trust depends on whether a line is known:
//
//   - With a line: take the UNION of every token's poles and let the caller's line filter prune
//     it. This is robust to noisy tokens ("Carlo", "Tommaso") because only poles actually served
//     by the line survive — exactly the "716 on Viale Carlo Tommaso Odescalchi" case.
//   - Without a line: pick the single most identifying token (the longest; ties broken by rarity),
//     since there is no line to prune away unrelated poles sharing a common word.
func resolvePlace(ctx context.Context, st *store.Store, query, route string) ([]store.Stop, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("empty place query")
	}
	if stops, err := st.SearchStopsByName(ctx, q, 0); err != nil {
		return nil, err
	} else if len(stops) > 0 {
		return stops, nil
	}
	tokens := significantStopTokens(q)
	if len(tokens) == 0 {
		return nil, nil
	}

	if route != "" {
		seen := map[string]bool{}
		union := []store.Stop{}
		for _, tok := range tokens {
			stops, err := st.SearchStopsByName(ctx, tok, 0)
			if err != nil {
				return nil, err
			}
			for _, s := range stops {
				if !seen[s.StopID] {
					seen[s.StopID] = true
					union = append(union, s)
				}
			}
		}
		return union, nil
	}

	var best []store.Stop
	bestTok := ""
	for _, tok := range tokens {
		stops, err := st.SearchStopsByName(ctx, tok, 0)
		if err != nil {
			return nil, err
		}
		if len(stops) == 0 {
			continue
		}
		// Prefer the longest token; on equal length the rarer (smaller) match set.
		if bestTok == "" || len(tok) > len(bestTok) || (len(tok) == len(bestTok) && len(stops) < len(best)) {
			best, bestTok = stops, tok
		}
	}
	return best, nil
}

// matchesDirection reports whether a pole's served lines include one matching the agent-supplied
// direction filter, which may be a numeric direction_id ("0"/"1") or a headsign substring.
func matchesDirection(lines []store.LineDirection, direction string) bool {
	d := strings.TrimSpace(direction)
	if d == "" {
		return true
	}
	if n, err := strconv.Atoi(d); err == nil {
		for _, ld := range lines {
			if ld.DirectionID == n {
				return true
			}
		}
		return false
	}
	dl := strings.ToLower(d)
	for _, ld := range lines {
		if strings.Contains(strings.ToLower(ld.Headsign), dl) {
			return true
		}
	}
	return false
}

// runArrivalsPlaceForMCP is the place-aware entry point behind the `arrivals` MCP tool. A numeric
// stop resolves to a single pole directly; a place name walks the line → direction → pole funnel,
// returning a structured {status:"ambiguous", ask:...} payload (never an error) whenever a choice
// is still open, so the stateless agent can ask the user the next question.
func runArrivalsPlaceForMCP(ctx context.Context, stop, route, direction string, limit, window int) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()

	// Numeric stop_id: exact single pole, no disambiguation.
	if isNumeric(stop) {
		s, err := a.store.StopByID(ctx, stop)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, fmt.Errorf("no stop with id %q", stop)
		}
		return arrivalsResponse(ctx, a, []store.Stop{*s}, route, direction, limit, window)
	}

	poles, err := resolvePlace(ctx, a.store, stop, route)
	if err != nil {
		return nil, err
	}
	if len(poles) == 0 {
		return nil, fmt.Errorf("no stop matches %q", stop)
	}

	ids := make([]string, len(poles))
	byID := make(map[string]store.Stop, len(poles))
	for i, p := range poles {
		ids[i] = p.StopID
		byID[p.StopID] = p
	}
	lines, err := a.store.LinesByStop(ctx, ids, route)
	if err != nil {
		return nil, err
	}
	// Keep only poles actually served (by the line, when given). LinesByStop omits poles with
	// no serving trip, so its keys are exactly the relevant set.
	relevant := make([]store.Stop, 0, len(lines))
	for id := range lines {
		relevant = append(relevant, byID[id])
	}
	sort.Slice(relevant, func(i, j int) bool { return relevant[i].StopName < relevant[j].StopName })
	if len(relevant) == 0 {
		if route != "" {
			return nil, fmt.Errorf("no stop matching %q is served by line %q", stop, route)
		}
		return nil, fmt.Errorf("no served stop matches %q", stop)
	}

	// Stage 1 — line. With no line given and several lines at the place, ask which line.
	if route == "" {
		distinct := distinctLines(lines)
		if len(distinct) > 1 {
			return ambiguousLines(stop, lines), nil
		}
		// Exactly one line at the place: adopt it implicitly and continue.
		route = distinct[0]
	}

	// Stage 2 — direction. Apply the agent's direction filter, then ask if still > 1 direction.
	if direction != "" {
		filtered := relevant[:0:0]
		for _, p := range relevant {
			if matchesDirection(lines[p.StopID], direction) {
				filtered = append(filtered, p)
			}
		}
		relevant = filtered
		if len(relevant) == 0 {
			return nil, fmt.Errorf("no %s pole at %q matches direction %q", route, stop, direction)
		}
	} else if dirs := distinctDirections(relevant, lines); len(dirs) > 1 {
		return ambiguousDirections(stop, route, relevant, lines), nil
	}

	// Stage 3 — pole. One pole, or a same-name cluster (aggregate), else ask which pole.
	switch {
	case len(relevant) == 1:
		return arrivalsResponse(ctx, a, relevant, route, direction, limit, window)
	case sameStopName(relevant):
		return arrivalsResponse(ctx, a, relevant, route, direction, limit, window)
	default:
		return ambiguousStops(stop, route, relevant, lines), nil
	}
}

// distinctLines returns the sorted distinct route_short_names across a pole→lines map.
func distinctLines(lines map[string][]store.LineDirection) []string {
	set := map[string]bool{}
	for _, lds := range lines {
		for _, ld := range lds {
			set[ld.RouteShortName] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// distinctDirections returns the distinct (direction_id, headsign) pairs across the given poles.
func distinctDirections(poles []store.Stop, lines map[string][]store.LineDirection) []store.LineDirection {
	seen := map[string]bool{}
	out := []store.LineDirection{}
	for _, p := range poles {
		for _, ld := range lines[p.StopID] {
			key := strconv.Itoa(ld.DirectionID) + "|" + ld.Headsign
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, store.LineDirection{Headsign: ld.Headsign, DirectionID: ld.DirectionID})
		}
	}
	return out
}

// sameStopName reports whether every pole shares an identical stop_name (a same-name cluster like
// Termini), in which case listing them would be useless and we aggregate instead.
func sameStopName(poles []store.Stop) bool {
	for i := 1; i < len(poles); i++ {
		if poles[i].StopName != poles[0].StopName {
			return false
		}
	}
	return len(poles) > 0
}

// --- ambiguous payload builders ---

type stopRef struct {
	StopID   string                `json:"stop_id"`
	StopName string                `json:"stop_name"`
	Lines    []store.LineDirection `json:"lines_served,omitempty"`
}

func ambiguousLines(place string, lines map[string][]store.LineDirection) map[string]any {
	// Group directions under each line short name.
	byLine := map[string][]store.LineDirection{}
	seen := map[string]bool{}
	for _, lds := range lines {
		for _, ld := range lds {
			key := ld.RouteShortName + "|" + strconv.Itoa(ld.DirectionID) + "|" + ld.Headsign
			if seen[key] {
				continue
			}
			seen[key] = true
			byLine[ld.RouteShortName] = append(byLine[ld.RouteShortName], store.LineDirection{Headsign: ld.Headsign, DirectionID: ld.DirectionID})
		}
	}
	type lineGroup struct {
		RouteShortName string                `json:"route_short_name"`
		Directions     []store.LineDirection `json:"directions"`
	}
	groups := make([]lineGroup, 0, len(byLine))
	for name, dirs := range byLine {
		groups = append(groups, lineGroup{RouteShortName: name, Directions: dirs})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].RouteShortName < groups[j].RouteShortName })
	return map[string]any{
		"status": "ambiguous",
		"ask":    "line",
		"place":  place,
		"lines":  groups,
		"hint":   fmt.Sprintf("Several lines serve %q. Re-call arrivals with route=<route_short_name>.", place),
	}
}

func ambiguousDirections(place, route string, poles []store.Stop, lines map[string][]store.LineDirection) map[string]any {
	type dirGroup struct {
		DirectionID int       `json:"direction_id"`
		Headsign    string    `json:"headsign"`
		Stops       []stopRef `json:"stops"`
	}
	byDir := map[string]*dirGroup{}
	order := []string{}
	for _, p := range poles {
		for _, ld := range lines[p.StopID] {
			key := strconv.Itoa(ld.DirectionID) + "|" + ld.Headsign
			g, ok := byDir[key]
			if !ok {
				g = &dirGroup{DirectionID: ld.DirectionID, Headsign: ld.Headsign}
				byDir[key] = g
				order = append(order, key)
			}
			g.Stops = append(g.Stops, stopRef{StopID: p.StopID, StopName: p.StopName})
		}
	}
	groups := make([]dirGroup, 0, len(order))
	for _, k := range order {
		groups = append(groups, *byDir[k])
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].DirectionID < groups[j].DirectionID })
	return map[string]any{
		"status":     "ambiguous",
		"ask":        "direction",
		"place":      place,
		"route":      route,
		"directions": groups,
		"hint":       fmt.Sprintf("Line %s serves %q in more than one direction. Re-call arrivals with direction=<direction_id or headsign>.", route, place),
	}
}

func ambiguousStops(place, route string, poles []store.Stop, lines map[string][]store.LineDirection) map[string]any {
	refs := make([]stopRef, 0, len(poles))
	for _, p := range poles {
		refs = append(refs, stopRef{StopID: p.StopID, StopName: p.StopName, Lines: lines[p.StopID]})
	}
	return map[string]any{
		"status": "ambiguous",
		"ask":    "stop",
		"place":  place,
		"route":  route,
		"stops":  refs,
		"hint":   fmt.Sprintf("Several poles match %q. Re-call arrivals with stop=<stop_id> of the one you want.", place),
	}
}

// --- arrivals across one or more poles ---

// arrivalsResponse builds the final arrivals payload for one resolved pole or an aggregated
// same-name cluster, fetching realtime once and merging it onto each pole's schedule.
func arrivalsResponse(ctx context.Context, a *app, poles []store.Stop, route, direction string, limit, window int) (any, error) {
	now := time.Now().In(a.loc)
	windowEnd := now.Add(time.Duration(window) * time.Minute)

	var rtLookup *realtime.TripUpdateLookup
	if tu, err := a.rt.TripUpdates(ctx); err == nil {
		rtLookup = realtime.BuildTripUpdateLookup(tu)
	}
	var vehByTrip map[string]string
	if vp, err := a.rt.VehiclePositions(ctx); err == nil {
		vehByTrip = realtime.VehicleByTrip(vp)
	}

	aggregate := len(poles) > 1
	rows := []arrivalRow{}
	for _, p := range poles {
		scheduled, err := scheduledInWindow(ctx, a.store, p.StopID, route, now, windowEnd)
		if err != nil {
			return nil, err
		}
		poleRows := mergeArrivals(scheduled, rtLookup, vehByTrip, p.StopID, now, windowEnd, route, a, ctx)
		if aggregate {
			for i := range poleRows {
				poleRows[i].StopID = p.StopID
			}
		}
		rows = append(rows, poleRows...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time.Before(rows[j].Time) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	var alerts []AlertView
	if msg, err := a.rt.ServiceAlerts(ctx); err == nil {
		all := collectAlerts(msg, a.store, now, ctx)
		alerts = filterRelevantAlerts(all, poles[0].StopID, rowsRouteShorts(rows), route)
	}

	resp := map[string]any{
		"arrivals": rows,
		"alerts":   alerts,
		"now":      now.Format(time.RFC3339),
	}
	if aggregate {
		stops := make([]map[string]string, len(poles))
		for i, p := range poles {
			stops[i] = map[string]string{"stop_id": p.StopID, "stop_name": p.StopName}
		}
		resp["stops"] = stops
		resp["aggregated"] = true
	} else {
		resp["stop"] = map[string]string{"stop_id": poles[0].StopID, "stop_name": poles[0].StopName}
	}
	return resp, nil
}
