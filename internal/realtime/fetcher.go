// Package realtime fetches and decodes GTFS-Realtime protobuf feeds from Roma Mobilità.
package realtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// Feed URLs.
const (
	TripUpdatesURL      = "https://romamobilita.it/sites/default/files/rome_rtgtfs_trip_updates_feed.pb"
	VehiclePositionsURL = "https://romamobilita.it/sites/default/files/rome_rtgtfs_vehicle_positions_feed.pb"
	ServiceAlertsURL    = "https://romamobilita.it/sites/default/files/rome_rtgtfs_service_alerts_feed.pb"

	httpTO = 10 * time.Second
)

// Fetcher provides cached access to the realtime feeds for the lifetime of a single
// CLI invocation. Each feed is fetched at most once per Fetcher.
type Fetcher struct {
	client *http.Client

	mu     sync.Mutex
	cached map[string]*gtfsrt.FeedMessage
	errs   map[string]error
}

// NewFetcher returns a Fetcher with the default HTTP client.
func NewFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{Timeout: httpTO},
		cached: map[string]*gtfsrt.FeedMessage{},
		errs:   map[string]error{},
	}
}

// TripUpdates returns the current trip_updates feed.
func (f *Fetcher) TripUpdates(ctx context.Context) (*gtfsrt.FeedMessage, error) {
	return f.fetch(ctx, TripUpdatesURL)
}

// VehiclePositions returns the current vehicle_positions feed.
func (f *Fetcher) VehiclePositions(ctx context.Context) (*gtfsrt.FeedMessage, error) {
	return f.fetch(ctx, VehiclePositionsURL)
}

// ServiceAlerts returns the current service_alerts feed.
func (f *Fetcher) ServiceAlerts(ctx context.Context) (*gtfsrt.FeedMessage, error) {
	return f.fetch(ctx, ServiceAlertsURL)
}

func (f *Fetcher) fetch(ctx context.Context, url string) (*gtfsrt.FeedMessage, error) {
	f.mu.Lock()
	if msg, ok := f.cached[url]; ok {
		f.mu.Unlock()
		return msg, nil
	}
	if err, ok := f.errs[url]; ok {
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()

	msg, err := doFetch(ctx, f.client, url)

	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.errs[url] = err
		return nil, err
	}
	f.cached[url] = msg
	return msg, nil
}

func doFetch(ctx context.Context, client *http.Client, url string) (*gtfsrt.FeedMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	msg := &gtfsrt.FeedMessage{}
	if err := proto.Unmarshal(body, msg); err != nil {
		return nil, fmt.Errorf("decode protobuf: %w", err)
	}
	return msg, nil
}

// TripUpdateLookup holds an indexed view of trip_updates by (trip_id, stop_id).
type TripUpdateLookup struct {
	// byTripStop maps trip_id → stop_id → predicted arrival/departure event.
	byTripStop map[string]map[string]*StopTimeEvent
	// byTrip maps trip_id → header-level delay (used as fallback when no per-stop event exists).
	byTrip map[string]int32
	// byTripVehicle maps trip_id → vehicle_id when the trip_update declares one.
	byTripVehicle map[string]string
	// timestamp from the feed header.
	Timestamp time.Time
}

// StopTimeEvent is a thin view of a GTFS-RT TripUpdate.StopTimeUpdate for a stop.
type StopTimeEvent struct {
	HasTime   bool
	Time      time.Time
	Delay     int32 // seconds
	HasDelay  bool
	Cancelled bool
	StopSeq   uint32
}

// BuildTripUpdateLookup indexes a trip_updates FeedMessage.
func BuildTripUpdateLookup(msg *gtfsrt.FeedMessage) *TripUpdateLookup {
	out := &TripUpdateLookup{
		byTripStop:    map[string]map[string]*StopTimeEvent{},
		byTrip:        map[string]int32{},
		byTripVehicle: map[string]string{},
	}
	if msg == nil {
		return out
	}
	if h := msg.GetHeader(); h != nil {
		out.Timestamp = time.Unix(int64(h.GetTimestamp()), 0)
	}
	for _, ent := range msg.GetEntity() {
		tu := ent.GetTripUpdate()
		if tu == nil {
			continue
		}
		tripID := tu.GetTrip().GetTripId()
		if tripID == "" {
			continue
		}
		if tu.Delay != nil {
			out.byTrip[tripID] = tu.GetDelay()
		}
		if v := tu.GetVehicle().GetId(); v != "" {
			out.byTripVehicle[tripID] = v
		}
		for _, stu := range tu.GetStopTimeUpdate() {
			stopID := stu.GetStopId()
			if stopID == "" {
				continue
			}
			ev := &StopTimeEvent{StopSeq: stu.GetStopSequence()}
			if stu.GetScheduleRelationship() == gtfsrt.TripUpdate_StopTimeUpdate_SKIPPED {
				ev.Cancelled = true
			}
			// Prefer arrival, fall back to departure.
			for _, src := range []*gtfsrt.TripUpdate_StopTimeEvent{stu.GetArrival(), stu.GetDeparture()} {
				if src == nil {
					continue
				}
				if src.Time != nil {
					ev.HasTime = true
					ev.Time = time.Unix(src.GetTime(), 0)
				}
				if src.Delay != nil {
					ev.HasDelay = true
					ev.Delay = src.GetDelay()
				}
				if ev.HasTime || ev.HasDelay {
					break
				}
			}
			m, ok := out.byTripStop[tripID]
			if !ok {
				m = map[string]*StopTimeEvent{}
				out.byTripStop[tripID] = m
			}
			m[stopID] = ev
		}
	}
	return out
}

// Get returns the stop-time event for (tripID, stopID), or nil if absent.
func (l *TripUpdateLookup) Get(tripID, stopID string) *StopTimeEvent {
	if l == nil {
		return nil
	}
	m, ok := l.byTripStop[tripID]
	if !ok {
		return nil
	}
	return m[stopID]
}

// TripDelay returns the trip-level delay (seconds, true) if known.
func (l *TripUpdateLookup) TripDelay(tripID string) (int32, bool) {
	if l == nil {
		return 0, false
	}
	d, ok := l.byTrip[tripID]
	return d, ok
}

// VehicleID returns the vehicle assigned to tripID by the trip_updates feed, or "" if none.
func (l *TripUpdateLookup) VehicleID(tripID string) string {
	if l == nil {
		return ""
	}
	return l.byTripVehicle[tripID]
}

// VehicleByTrip extracts a trip_id → vehicle_id map from a vehicle_positions FeedMessage.
// Used as a fallback when trip_updates don't include vehicle descriptors.
func VehicleByTrip(msg *gtfsrt.FeedMessage) map[string]string {
	out := map[string]string{}
	if msg == nil {
		return out
	}
	for _, ent := range msg.GetEntity() {
		vp := ent.GetVehicle()
		if vp == nil {
			continue
		}
		tripID := vp.GetTrip().GetTripId()
		vehID := vp.GetVehicle().GetId()
		if tripID != "" && vehID != "" {
			out[tripID] = vehID
		}
	}
	return out
}

// TripStops returns the set of (tripID, stopID) keys present in the feed. Used to render
// realtime-only rows that don't appear in the schedule for the chosen window.
func (l *TripUpdateLookup) TripStops() map[string][]string {
	if l == nil {
		return nil
	}
	out := make(map[string][]string, len(l.byTripStop))
	for trip, stops := range l.byTripStop {
		for s := range stops {
			out[trip] = append(out[trip], s)
		}
	}
	return out
}
