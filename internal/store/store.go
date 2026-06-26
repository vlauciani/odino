// Package store wraps the SQLite cache that holds parsed GTFS static data.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a thin wrapper around the SQLite database holding the GTFS static cache.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) the SQLite database at path. Schema is applied if missing.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite is safe but a single writer simplifies things
	s := &Store{db: db, path: path}
	if err := s.applySchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying connection for advanced queries.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the on-disk path of the SQLite file.
func (s *Store) Path() string { return s.path }

func (s *Store) applySchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS agency (
			agency_id   TEXT PRIMARY KEY,
			agency_name TEXT NOT NULL,
			agency_url  TEXT,
			agency_tz   TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS stops (
			stop_id   TEXT PRIMARY KEY,
			stop_code TEXT,
			stop_name TEXT,
			stop_lat  REAL,
			stop_lon  REAL,
			location_type INTEGER,
			parent_station TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_stops_name ON stops(stop_name COLLATE NOCASE)`,
		`CREATE TABLE IF NOT EXISTS routes (
			route_id         TEXT PRIMARY KEY,
			agency_id        TEXT,
			route_short_name TEXT,
			route_long_name  TEXT,
			route_type       INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_routes_short ON routes(route_short_name)`,
		`CREATE TABLE IF NOT EXISTS trips (
			trip_id       TEXT PRIMARY KEY,
			route_id      TEXT NOT NULL,
			service_id    TEXT,
			trip_headsign TEXT,
			direction_id  INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_trips_route ON trips(route_id)`,
		`CREATE INDEX IF NOT EXISTS idx_trips_service ON trips(service_id)`,
		`CREATE TABLE IF NOT EXISTS stop_times (
			trip_id        TEXT NOT NULL,
			stop_id        TEXT NOT NULL,
			arrival_time   INTEGER,
			departure_time INTEGER,
			stop_sequence  INTEGER NOT NULL,
			PRIMARY KEY (trip_id, stop_sequence)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_stop_times_stop ON stop_times(stop_id, departure_time)`,
		`CREATE INDEX IF NOT EXISTS idx_stop_times_trip ON stop_times(trip_id)`,
		`CREATE TABLE IF NOT EXISTS calendar (
			service_id TEXT PRIMARY KEY,
			monday    INTEGER, tuesday   INTEGER, wednesday INTEGER,
			thursday  INTEGER, friday    INTEGER, saturday  INTEGER, sunday INTEGER,
			start_date TEXT, end_date TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS calendar_dates (
			service_id     TEXT NOT NULL,
			date           TEXT NOT NULL,
			exception_type INTEGER NOT NULL,
			PRIMARY KEY (service_id, date)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("schema: %w: %s", err, q)
		}
	}
	return nil
}

// Meta returns the value of a metadata key (empty string if missing).
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta upserts a metadata key.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// LastUpdated returns the time the cache was last populated, zero if never.
func (s *Store) LastUpdated() (time.Time, error) {
	v, err := s.Meta("last_updated")
	if err != nil || v == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, v)
}

// MarkUpdated records the cache update time as 'now'.
func (s *Store) MarkUpdated(now time.Time, md5sum string) error {
	if err := s.SetMeta("last_updated", now.Format(time.RFC3339)); err != nil {
		return err
	}
	return s.SetMeta("md5", md5sum)
}

// Truncate wipes the GTFS static tables (used before a fresh import).
func (s *Store) Truncate(ctx context.Context) error {
	for _, t := range []string{"stop_times", "trips", "routes", "stops", "calendar", "calendar_dates", "agency"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	return nil
}

// --- domain queries ---

// Stop is a GTFS stop row.
type Stop struct {
	StopID   string  `json:"stop_id"`
	StopCode string  `json:"stop_code,omitempty"`
	StopName string  `json:"stop_name"`
	StopLat  float64 `json:"stop_lat"`
	StopLon  float64 `json:"stop_lon"`
}

// LineDirection is one line serving a pole, in one direction of travel. Headsign is the
// destination shown on the vehicle (GTFS trip_headsign); DirectionID is GTFS direction_id.
type LineDirection struct {
	RouteShortName string `json:"route_short_name,omitempty"`
	Headsign       string `json:"headsign,omitempty"`
	DirectionID    int    `json:"direction_id"`
}

// LinesByStop returns, for each requested stop_id, the distinct lines+directions that serve
// it. When routeShort is non-empty, only that line is considered. The result is keyed by
// stop_id; stops with no serving trip are simply absent from the map. Within each stop the
// lines are de-duplicated on (route_short_name, direction_id, headsign) and ordered.
func (s *Store) LinesByStop(ctx context.Context, stopIDs []string, routeShort string) (map[string][]LineDirection, error) {
	if len(stopIDs) == 0 {
		return map[string][]LineDirection{}, nil
	}
	ph := make([]string, len(stopIDs))
	args := make([]any, 0, len(stopIDs)+1)
	for i, id := range stopIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	q := `SELECT DISTINCT st.stop_id, IFNULL(r.route_short_name,''),
	             IFNULL(t.trip_headsign,''), IFNULL(t.direction_id,0)
	      FROM stop_times st
	      JOIN trips t  ON t.trip_id  = st.trip_id
	      JOIN routes r ON r.route_id = t.route_id
	      WHERE st.stop_id IN (` + strings.Join(ph, ",") + `)`
	if routeShort != "" {
		q += ` AND LOWER(r.route_short_name) = LOWER(?)`
		args = append(args, routeShort)
	}
	q += ` ORDER BY r.route_short_name, t.direction_id, t.trip_headsign`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]LineDirection{}
	for rows.Next() {
		var sid string
		var ld LineDirection
		if err := rows.Scan(&sid, &ld.RouteShortName, &ld.Headsign, &ld.DirectionID); err != nil {
			return nil, err
		}
		// The DISTINCT in SQL already collapses duplicates; ORDER BY keeps them grouped.
		out[sid] = append(out[sid], ld)
	}
	return out, rows.Err()
}

// StopByID returns the stop with the given stop_id, or nil if missing.
func (s *Store) StopByID(ctx context.Context, id string) (*Stop, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT stop_id, IFNULL(stop_code,''), IFNULL(stop_name,''), IFNULL(stop_lat,0), IFNULL(stop_lon,0)
		 FROM stops WHERE stop_id=?`, id)
	var v Stop
	if err := row.Scan(&v.StopID, &v.StopCode, &v.StopName, &v.StopLat, &v.StopLon); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

// SearchStopsByName returns stops whose name contains q (case-insensitive). Limit 0 == no cap.
func (s *Store) SearchStopsByName(ctx context.Context, q string, limit int) ([]Stop, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	args := []any{"%" + strings.ToLower(q) + "%"}
	sqlq := `SELECT stop_id, IFNULL(stop_code,''), IFNULL(stop_name,''), IFNULL(stop_lat,0), IFNULL(stop_lon,0)
	         FROM stops
	         WHERE LOWER(stop_name) LIKE ?
	         ORDER BY stop_name`
	if limit > 0 {
		sqlq += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stop
	for rows.Next() {
		var v Stop
		if err := rows.Scan(&v.StopID, &v.StopCode, &v.StopName, &v.StopLat, &v.StopLon); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// StopNearby is a Stop plus its distance (in meters) from a query point.
type StopNearby struct {
	Stop
	DistanceM int `json:"distance_m"`
}

// StopsNearby returns stops within radiusM meters of (lat, lon), sorted by ascending
// distance. If routeShortName is non-empty, only stops served by at least one trip on
// that line are returned. limit caps the result; 0 = no cap.
func (s *Store) StopsNearby(ctx context.Context, lat, lon float64, radiusM int, routeShortName string, limit int) ([]StopNearby, error) {
	if radiusM <= 0 {
		return nil, fmt.Errorf("radius must be > 0 meters")
	}
	// Bounding box around (lat, lon). 1° lat ≈ 111_320 m everywhere; 1° lon at latitude
	// φ ≈ 111_320 · cos φ. We use a slightly inflated factor to avoid edge misses.
	const metersPerDegLat = 111320.0
	latRad := lat * math.Pi / 180.0
	metersPerDegLon := metersPerDegLat * math.Cos(latRad)
	if metersPerDegLon < 1 {
		metersPerDegLon = 1
	}
	latDelta := float64(radiusM) / metersPerDegLat
	lonDelta := float64(radiusM) / metersPerDegLon

	args := []any{
		lat - latDelta, lat + latDelta,
		lon - lonDelta, lon + lonDelta,
	}
	q := `SELECT stop_id, IFNULL(stop_code,''), IFNULL(stop_name,''),
	             IFNULL(stop_lat,0), IFNULL(stop_lon,0)
	      FROM stops
	      WHERE stop_lat BETWEEN ? AND ?
	        AND stop_lon BETWEEN ? AND ?`
	if routeShortName != "" {
		q += ` AND EXISTS (
		  SELECT 1
		  FROM stop_times st
		  JOIN trips t  ON t.trip_id = st.trip_id
		  JOIN routes r ON r.route_id = t.route_id
		  WHERE st.stop_id = stops.stop_id
		    AND LOWER(r.route_short_name) = LOWER(?)
		)`
		args = append(args, routeShortName)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StopNearby{}
	for rows.Next() {
		var v StopNearby
		if err := rows.Scan(&v.StopID, &v.StopCode, &v.StopName, &v.StopLat, &v.StopLon); err != nil {
			return nil, err
		}
		d := haversineMeters(lat, lon, v.StopLat, v.StopLon)
		if d > float64(radiusM) {
			continue
		}
		v.DistanceM = int(d + 0.5)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DistanceM < out[j].DistanceM })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// haversineMeters returns the great-circle distance between two WGS84 points, in metres.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const earthR = 6371000.0 // metres
	φ1 := lat1 * math.Pi / 180
	φ2 := lat2 * math.Pi / 180
	dφ := (lat2 - lat1) * math.Pi / 180
	dλ := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dφ/2)*math.Sin(dφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(dλ/2)*math.Sin(dλ/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthR * c
}

// Route is a GTFS route row.
type Route struct {
	RouteID        string `json:"route_id"`
	AgencyID       string `json:"agency_id,omitempty"`
	RouteShortName string `json:"route_short_name"`
	RouteLongName  string `json:"route_long_name,omitempty"`
	RouteType      int    `json:"route_type"`
}

// ListRoutes returns all routes.
func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT route_id, IFNULL(agency_id,''), IFNULL(route_short_name,''), IFNULL(route_long_name,''), IFNULL(route_type,0)
		 FROM routes ORDER BY route_short_name, route_long_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.RouteID, &r.AgencyID, &r.RouteShortName, &r.RouteLongName, &r.RouteType); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RouteIDsByShortName returns route_ids matching a route_short_name (case-insensitive).
func (s *Store) RouteIDsByShortName(ctx context.Context, shortName string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT route_id FROM routes WHERE LOWER(route_short_name)=LOWER(?)`, shortName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ScheduledArrival represents a planned arrival at a stop derived from GTFS static.
type ScheduledArrival struct {
	TripID         string
	RouteID        string
	RouteShortName string
	RouteLongName  string
	TripHeadsign   string
	DirectionID    int
	StopSequence   int
	ArrivalTime    int // seconds since service-day midnight
	DepartureTime  int
}

// ScheduledArrivalsAt returns planned arrivals at stopID whose departure_time is between
// fromSec and toSec seconds-since-service-day. routeShortName filters by route_short_name
// when non-empty (any matching variant); serviceIDs restricts to active services for the
// service day.
func (s *Store) ScheduledArrivalsAt(ctx context.Context, stopID string, serviceIDs []string, fromSec, toSec int, routeShortName string, limit int) ([]ScheduledArrival, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(serviceIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := []any{stopID}
	for _, sid := range serviceIDs {
		args = append(args, sid)
	}
	args = append(args, fromSec, toSec)
	q := `SELECT st.trip_id, r.route_id, IFNULL(r.route_short_name,''), IFNULL(r.route_long_name,''),
	             IFNULL(t.trip_headsign,''), IFNULL(t.direction_id,0),
	             st.stop_sequence, IFNULL(st.arrival_time, st.departure_time), st.departure_time
	      FROM stop_times st
	      JOIN trips t ON t.trip_id = st.trip_id
	      JOIN routes r ON r.route_id = t.route_id
	      WHERE st.stop_id = ?
	        AND t.service_id IN (` + placeholders + `)
	        AND st.departure_time BETWEEN ? AND ?`
	if routeShortName != "" {
		q += " AND LOWER(r.route_short_name) = LOWER(?)"
		args = append(args, routeShortName)
	}
	q += " ORDER BY st.departure_time"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledArrival
	for rows.Next() {
		var a ScheduledArrival
		if err := rows.Scan(&a.TripID, &a.RouteID, &a.RouteShortName, &a.RouteLongName,
			&a.TripHeadsign, &a.DirectionID, &a.StopSequence, &a.ArrivalTime, &a.DepartureTime); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TripInfo holds the trip+route metadata used to render an arrival row from a realtime-only trip_update.
type TripInfo struct {
	RouteID        string
	RouteShortName string
	RouteLongName  string
	TripHeadsign   string
	DirectionID    int
}

// TripInfoByID returns the route+headsign for the given trip_id, or nil if not found.
func (s *Store) TripInfoByID(ctx context.Context, tripID string) (*TripInfo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT r.route_id, IFNULL(r.route_short_name,''), IFNULL(r.route_long_name,''),
		        IFNULL(t.trip_headsign,''), IFNULL(t.direction_id,0)
		 FROM trips t JOIN routes r ON r.route_id = t.route_id
		 WHERE t.trip_id = ?`, tripID)
	var v TripInfo
	if err := row.Scan(&v.RouteID, &v.RouteShortName, &v.RouteLongName, &v.TripHeadsign, &v.DirectionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

// TripStop is one ordered stop on a trip's schedule, joined with stop metadata.
type TripStop struct {
	StopSequence int     `json:"stop_sequence"`
	StopID       string  `json:"stop_id"`
	StopName     string  `json:"stop_name"`
	StopLat      float64 `json:"stop_lat"`
	StopLon      float64 `json:"stop_lon"`
	ArrivalSec   int     `json:"arrival_sec"` // seconds since service-day midnight
	DepartureSec int     `json:"departure_sec"`
}

// TripScheduleByID returns the trip metadata plus the ordered stop sequence for tripID,
// joined with stop names and coordinates. Returns nil, nil when the trip is unknown.
func (s *Store) TripScheduleByID(ctx context.Context, tripID string) (*TripInfo, []TripStop, error) {
	info, err := s.TripInfoByID(ctx, tripID)
	if err != nil || info == nil {
		return nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT st.stop_sequence,
		       st.stop_id, IFNULL(s.stop_name,''), IFNULL(s.stop_lat,0), IFNULL(s.stop_lon,0),
		       st.arrival_time, st.departure_time
		FROM stop_times st
		JOIN stops s ON s.stop_id = st.stop_id
		WHERE st.trip_id = ?
		ORDER BY st.stop_sequence`, tripID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var stops []TripStop
	for rows.Next() {
		var ts TripStop
		if err := rows.Scan(&ts.StopSequence, &ts.StopID, &ts.StopName, &ts.StopLat, &ts.StopLon, &ts.ArrivalSec, &ts.DepartureSec); err != nil {
			return nil, nil, err
		}
		stops = append(stops, ts)
	}
	return info, stops, rows.Err()
}

// ScheduledStopTime returns the planned departure_time (seconds since service midnight)
// for the given trip_id at stop_id. Returns -1 if no match.
func (s *Store) ScheduledStopTime(ctx context.Context, tripID, stopID string) (int, error) {
	var dep int
	err := s.db.QueryRowContext(ctx,
		`SELECT departure_time FROM stop_times WHERE trip_id=? AND stop_id=?`, tripID, stopID).Scan(&dep)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	return dep, err
}

// StopNameByID returns the stop_name for stop_id, empty string if missing.
func (s *Store) StopNameByID(ctx context.Context, id string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT IFNULL(stop_name,'') FROM stops WHERE stop_id=?`, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// ActiveServiceIDs returns service_ids active on the given date (YYYYMMDD) considering
// both calendar weekday flags and calendar_dates exceptions.
func (s *Store) ActiveServiceIDs(ctx context.Context, date time.Time) ([]string, error) {
	dateStr := date.Format("20060102")
	weekday := date.Weekday()
	col := []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}[int(weekday)]

	// Base: services active in calendar.txt with weekday flag set and date in range.
	rows, err := s.db.QueryContext(ctx,
		`SELECT service_id FROM calendar
		 WHERE `+col+` = 1
		   AND (start_date = '' OR start_date <= ?)
		   AND (end_date = '' OR end_date >= ?)`, dateStr, dateStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	active := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		active[s] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Apply calendar_dates exceptions: type 1 = added, type 2 = removed.
	exRows, err := s.db.QueryContext(ctx,
		`SELECT service_id, exception_type FROM calendar_dates WHERE date = ?`, dateStr)
	if err != nil {
		return nil, err
	}
	defer exRows.Close()
	for exRows.Next() {
		var sid string
		var typ int
		if err := exRows.Scan(&sid, &typ); err != nil {
			return nil, err
		}
		switch typ {
		case 1:
			active[sid] = true
		case 2:
			delete(active, sid)
		}
	}
	if err := exRows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(active))
	for k := range active {
		out = append(out, k)
	}
	return out, nil
}
