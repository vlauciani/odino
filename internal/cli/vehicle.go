package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"github.com/spf13/cobra"

	"github.com/vlauciani/odino/internal/output"
	"github.com/vlauciani/odino/internal/realtime"
)

// vehicleFollowRow is one upcoming stop in the vehicle's current trip.
type vehicleFollowRow struct {
	StopSequence int       `json:"stop_sequence"`
	Time         time.Time `json:"time"`
	Min          int       `json:"minutes"`
	StopID       string    `json:"stop_id"`
	StopName     string    `json:"stop_name"`
	Source       string    `json:"source"` // LIVE | SCHED
}

// vehicleFollowView is the top-level shape (used both for table header and JSON).
type vehicleFollowView struct {
	VehicleID      string             `json:"vehicle_id"`
	RouteShortName string             `json:"route_short_name"`
	RouteID        string             `json:"route_id"`
	TripID         string             `json:"trip_id"`
	Headsign       string             `json:"headsign"`
	CurrentStatus  string             `json:"current_status,omitempty"`
	CurrentStopID  string             `json:"current_stop_id,omitempty"`
	CurrentStopSeq uint32             `json:"current_stop_sequence,omitempty"`
	Position       *vehiclePosition   `json:"position,omitempty"`
	UpdatedAt      time.Time          `json:"updated_at"`
	UpcomingStops  []vehicleFollowRow `json:"upcoming_stops"`
	Destination    *destinationInfo   `json:"destination,omitempty"`
}

// destinationInfo summarizes the ride from "now" to a user-chosen --to stop.
type destinationInfo struct {
	StopID      string    `json:"stop_id"`
	StopName    string    `json:"stop_name"`
	StopLat     float64   `json:"stop_lat,omitempty"`
	StopLon     float64   `json:"stop_lon,omitempty"`
	ETA         time.Time `json:"eta"`
	MinutesAway int       `json:"minutes"`
	StopsAway   int       `json:"stops"`  // counts upcoming stops including the destination
	Source      string    `json:"source"` // LIVE | SCHED for the destination's prediction
}

type vehiclePosition struct {
	Lat     float32  `json:"lat"`
	Lon     float32  `json:"lon"`
	Bearing *float32 `json:"bearing"`
}

func newVehicleCmd(flags *rootFlags) *cobra.Command {
	var (
		limit  int
		toStop string
	)
	cmd := &cobra.Command{
		Use:   "vehicle <vehicle_id>",
		Short: "Follow a specific vehicle across its remaining stops.",
		Long: `Locate a vehicle currently in service by its vehicle_id and show every
remaining stop on its current trip with predicted times (LIVE when the realtime
feed publishes a per-stop prediction, SCHED when only the planned schedule
applies).

Use --to <stop_id> to focus on a destination: the table is trimmed to end at
that stop, and a summary line reports ETA, minutes left, and number of stops
to go.

Find vehicle_ids via 'odino vehicles --route <line>' or in the 'vehicle'
column of 'odino arrivals'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errW := stdIO(cmd)
			a, err := buildApp(cmd.Context(), flags, false, out, errW)
			if err != nil {
				return err
			}
			defer a.close()
			return runVehicleFollow(cmd.Context(), a, args[0], toStop, limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum number of upcoming stops to show (0 = all remaining). Ignored when --to is set.")
	cmd.Flags().StringVar(&toStop, "to", "", "Destination stop_id; trim the upcoming-stops list to end at this stop and report ETA + stops remaining.")
	return cmd
}

func runVehicleFollow(ctx context.Context, a *app, vehicleID, toStopID string, limit int) error {
	view, err := buildVehicleFollowView(ctx, a, vehicleID, toStopID, limit)
	if err != nil {
		return err
	}
	if a.flags.asJSON {
		return output.JSON(a.out, view)
	}
	// Human header.
	fmt.Fprintf(a.out, "Vehicle %s — line %s → %s\n", view.VehicleID, view.RouteShortName, view.Headsign)
	if view.Position != nil {
		fmt.Fprintf(a.out, "Position: %.6f, %.6f", view.Position.Lat, view.Position.Lon)
		if view.Position.Bearing != nil {
			fmt.Fprintf(a.out, " (bearing %.0f°)", *view.Position.Bearing)
		}
		fmt.Fprintln(a.out)
	}
	if view.CurrentStatus != "" || view.CurrentStopID != "" {
		status := view.CurrentStatus
		if status == "" {
			status = "near"
		}
		stopLabel := view.CurrentStopID
		if name, _ := a.store.StopNameByID(ctx, view.CurrentStopID); name != "" {
			stopLabel = fmt.Sprintf("%s (%s)", view.CurrentStopID, name)
		}
		fmt.Fprintf(a.out, "Status: %s %s — updated %s\n",
			status, stopLabel, view.UpdatedAt.In(a.loc).Format("15:04:05"))
	}
	fmt.Fprintf(a.out, "Trip: %s\n", view.TripID)
	if view.Destination != nil {
		d := view.Destination
		fmt.Fprintf(a.out, "Destination: %s (%s) — ETA %s (%d min, %d %s away, %s)\n",
			d.StopName, d.StopID,
			d.ETA.In(a.loc).Format("15:04"),
			d.MinutesAway,
			d.StopsAway, pluralize(d.StopsAway, "stop", "stops"),
			d.Source)
	}
	fmt.Fprintln(a.out)

	if len(view.UpcomingStops) == 0 {
		fmt.Fprintln(a.out, "(no upcoming stops on this trip)")
		return nil
	}
	rows := make([][]string, 0, len(view.UpcomingStops))
	for _, s := range view.UpcomingStops {
		rows = append(rows, []string{
			strconv.Itoa(s.StopSequence),
			s.Time.In(a.loc).Format("15:04"),
			strconv.Itoa(s.Min),
			s.StopID,
			truncate(s.StopName, 40),
			s.Source,
		})
	}
	return output.Table(a.out, []string{"seq", "time", "min", "stop_id", "stop_name", "src"}, rows)
}

func buildVehicleFollowView(ctx context.Context, a *app, vehicleID, toStopID string, limit int) (*vehicleFollowView, error) {
	vp, err := a.rt.VehiclePositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching vehicle positions: %w", err)
	}
	ent := findVehicleEntity(vp, vehicleID)
	if ent == nil {
		return nil, fmt.Errorf("vehicle %q not currently in service (no entry in vehicle_positions feed)", vehicleID)
	}
	pos := ent.GetPosition()
	tripID := ent.GetTrip().GetTripId()
	if tripID == "" {
		return nil, fmt.Errorf("vehicle %q is in service but has no trip assigned", vehicleID)
	}

	info, stops, err := a.store.TripScheduleByID(ctx, tripID)
	if err != nil {
		return nil, fmt.Errorf("loading trip schedule: %w", err)
	}
	if info == nil {
		// Vehicle is on a trip the static GTFS doesn't know about (added trip, or stale cache).
		return nil, fmt.Errorf("trip %q for vehicle %q not found in the local cache — try 'odino update'", tripID, vehicleID)
	}

	tu, _ := a.rt.TripUpdates(ctx)
	rtLookup := realtime.BuildTripUpdateLookup(tu)

	now := time.Now().In(a.loc)
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, a.loc)

	// Filter to upcoming stops: stop_sequence > current_stop_sequence,
	// OR include the current stop when the vehicle is INCOMING_AT / STOPPED_AT it.
	curSeq := ent.GetCurrentStopSequence()
	curStatus := ent.GetCurrentStatus()
	includeCurrent := curStatus == gtfsrt.VehiclePosition_STOPPED_AT ||
		curStatus == gtfsrt.VehiclePosition_INCOMING_AT

	upcoming := make([]vehicleFollowRow, 0, len(stops))
	for _, ts := range stops {
		if uint32(ts.StopSequence) < curSeq {
			continue
		}
		if uint32(ts.StopSequence) == curSeq && !includeCurrent {
			continue
		}
		// Planned time anchored to today; GTFS allows >=24:00 for next-day extended hours.
		predicted := base.Add(time.Duration(ts.DepartureSec) * time.Second)
		source := "SCHED"
		if rtLookup != nil {
			if ev := rtLookup.Get(tripID, ts.StopID); ev != nil {
				if ev.Cancelled {
					continue
				}
				if ev.HasTime {
					predicted = ev.Time
					source = "LIVE"
				} else if ev.HasDelay {
					predicted = predicted.Add(time.Duration(ev.Delay) * time.Second)
					source = "LIVE"
				}
			} else if d, ok := rtLookup.TripDelay(tripID); ok {
				predicted = predicted.Add(time.Duration(d) * time.Second)
				source = "LIVE"
			}
		}
		minutes := int(predicted.Sub(now).Minutes())
		// Surface even slightly-past stops the vehicle hasn't cleared yet (within grace).
		if predicted.Before(now.Add(-2 * time.Minute)) {
			continue
		}
		upcoming = append(upcoming, vehicleFollowRow{
			StopSequence: ts.StopSequence,
			Time:         predicted,
			Min:          minutes,
			StopID:       ts.StopID,
			StopName:     ts.StopName,
			Source:       source,
		})
	}
	// --to: trim upcoming at the destination and build the summary.
	var destination *destinationInfo
	if toStopID != "" {
		// Validate the destination is on this trip's path.
		var destSeq int
		foundInTrip := false
		var destLat, destLon float64
		var destName string
		for _, ts := range stops {
			if ts.StopID == toStopID {
				foundInTrip = true
				destSeq = ts.StopSequence
				destLat, destLon = ts.StopLat, ts.StopLon
				destName = ts.StopName
				break
			}
		}
		if !foundInTrip {
			return nil, fmt.Errorf("stop %q is not on the route of vehicle %s's current trip (line %s, trip %s)",
				toStopID, vehicleID, info.RouteShortName, tripID)
		}
		if uint32(destSeq) < curSeq {
			return nil, fmt.Errorf("vehicle %s has already passed stop %q (stop_sequence %d, current %d)",
				vehicleID, toStopID, destSeq, curSeq)
		}
		// Locate the destination inside the upcoming slice and truncate.
		cutIdx := -1
		for i, r := range upcoming {
			if r.StopID == toStopID {
				cutIdx = i
				break
			}
		}
		if cutIdx >= 0 {
			upcoming = upcoming[:cutIdx+1]
			d := upcoming[cutIdx]
			destination = &destinationInfo{
				StopID:      d.StopID,
				StopName:    d.StopName,
				StopLat:     destLat,
				StopLon:     destLon,
				ETA:         d.Time,
				MinutesAway: d.Min,
				StopsAway:   cutIdx + 1,
				Source:      d.Source,
			}
		} else {
			// The destination is on the trip and after current, but the rendered upcoming
			// list dropped it (e.g. predicted time fell into the past grace window).
			return nil, fmt.Errorf("destination stop %q (%s) appears past the predicted window for vehicle %s — try again in a few seconds",
				toStopID, destName, vehicleID)
		}
		// --limit is intentionally ignored when --to is set; the destination IS the cap.
	} else if limit > 0 && len(upcoming) > limit {
		upcoming = upcoming[:limit]
	}

	view := &vehicleFollowView{
		VehicleID:      ent.GetVehicle().GetId(),
		RouteShortName: info.RouteShortName,
		RouteID:        info.RouteID,
		TripID:         tripID,
		Headsign:       info.TripHeadsign,
		CurrentStatus:  vehicleStatusName(curStatus),
		CurrentStopID:  ent.GetStopId(),
		CurrentStopSeq: curSeq,
		UpdatedAt:      time.Unix(int64(ent.GetTimestamp()), 0),
		UpcomingStops:  upcoming,
	}
	if pos != nil {
		view.Position = &vehiclePosition{Lat: pos.GetLatitude(), Lon: pos.GetLongitude()}
		if pos.Bearing != nil {
			b := *pos.Bearing
			view.Position.Bearing = &b
		}
	}
	view.Destination = destination
	return view, nil
}

// pluralize picks singular/plural based on n.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// findVehicleEntity returns the *VehiclePosition for vehicle_id, or nil if absent.
func findVehicleEntity(msg *gtfsrt.FeedMessage, vehicleID string) *gtfsrt.VehiclePosition {
	if msg == nil {
		return nil
	}
	for _, ent := range msg.GetEntity() {
		vp := ent.GetVehicle()
		if vp == nil {
			continue
		}
		if strings.EqualFold(vp.GetVehicle().GetId(), vehicleID) {
			return vp
		}
	}
	return nil
}

func vehicleStatusName(s gtfsrt.VehiclePosition_VehicleStopStatus) string {
	switch s {
	case gtfsrt.VehiclePosition_INCOMING_AT:
		return "INCOMING_AT"
	case gtfsrt.VehiclePosition_STOPPED_AT:
		return "STOPPED_AT"
	case gtfsrt.VehiclePosition_IN_TRANSIT_TO:
		return "IN_TRANSIT_TO"
	default:
		return ""
	}
}

// runVehicleFollowForMCP is the MCP-side wrapper.
func runVehicleFollowForMCP(ctx context.Context, vehicleID, toStopID string, limit int) (any, error) {
	a, err := mcpApp(ctx, false)
	if err != nil {
		return nil, err
	}
	defer a.close()
	return buildVehicleFollowView(ctx, a, vehicleID, toStopID, limit)
}

