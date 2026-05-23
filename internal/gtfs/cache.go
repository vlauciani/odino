// Package gtfs manages the on-disk cache of the GTFS static feed and its ingestion
// into the SQLite store.
package gtfs

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/store"
)

// Roma Mobilità GTFS endpoints. The realtime URLs live in the realtime package.
const (
	StaticZipURL = "https://romamobilita.it/sites/default/files/rome_static_gtfs.zip"
	StaticMD5URL = "https://romamobilita.it/sites/default/files/rome_static_gtfs.zip.md5"

	defaultTTL = 24 * time.Hour
	httpTO     = 60 * time.Second // static zip is ~30MB; allow more than RT
)

// Cache is the on-disk cache of the GTFS static feed.
type Cache struct {
	Dir string
}

// NewCache returns a cache rooted at the resolved cache directory. Honours
// ODINO_CACHE_DIR; defaults to $XDG_CACHE_HOME/odino or ~/.cache/odino.
func NewCache() (*Cache, error) {
	dir := os.Getenv("ODINO_CACHE_DIR")
	if dir == "" {
		base := os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve cache dir: %w", err)
			}
			base = filepath.Join(home, ".cache")
		}
		dir = filepath.Join(base, "odino")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	return &Cache{Dir: dir}, nil
}

// DBPath is the SQLite database path inside the cache.
func (c *Cache) DBPath() string { return filepath.Join(c.Dir, "gtfs.db") }

// zipPath is the local copy of rome_static_gtfs.zip.
func (c *Cache) zipPath() string { return filepath.Join(c.Dir, "rome_static_gtfs.zip") }

// md5Path is the cached md5 sum.
func (c *Cache) md5Path() string { return filepath.Join(c.Dir, "rome_static_gtfs.zip.md5") }

// EnsureFresh ensures the SQLite cache exists and is at most maxAge old (default 24h).
// Returns whether a refresh happened. If force is true, the cache is rebuilt unconditionally.
func (c *Cache) EnsureFresh(ctx context.Context, st *store.Store, maxAge time.Duration, force bool, log io.Writer) (bool, error) {
	if maxAge <= 0 {
		maxAge = defaultTTL
	}
	if !force {
		last, err := st.LastUpdated()
		if err == nil && !last.IsZero() && time.Since(last) < maxAge {
			return false, nil
		}
	}
	if err := c.refresh(ctx, st, force, log); err != nil {
		return false, err
	}
	return true, nil
}

// refresh downloads the zip (skipping re-parse if md5 unchanged) and rebuilds the SQLite cache.
func (c *Cache) refresh(ctx context.Context, st *store.Store, force bool, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	fmt.Fprintln(log, "Updating GTFS cache…")

	// Fetch upstream md5 (best-effort: continue even if missing).
	remoteMD5, _ := fetchString(ctx, StaticMD5URL)
	remoteMD5 = strings.TrimSpace(strings.Fields(remoteMD5+" ")[0]) // strip filename in `md5 zip` format

	// Decide whether we can skip the download by comparing with on-disk zip.
	zipExists := false
	if fi, err := os.Stat(c.zipPath()); err == nil && fi.Size() > 0 {
		zipExists = true
	}
	needDownload := true
	if zipExists && remoteMD5 != "" {
		if got, err := fileMD5(c.zipPath()); err == nil && strings.EqualFold(got, remoteMD5) {
			needDownload = false
		}
	}

	if needDownload {
		fmt.Fprintln(log, "  downloading static feed…")
		if err := downloadFile(ctx, StaticZipURL, c.zipPath()); err != nil {
			return fmt.Errorf("download static gtfs: %w", err)
		}
	}

	// Compute md5 of the local zip (authoritative for the meta record).
	localMD5, err := fileMD5(c.zipPath())
	if err != nil {
		return fmt.Errorf("md5 local zip: %w", err)
	}
	if remoteMD5 != "" {
		_ = os.WriteFile(c.md5Path(), []byte(remoteMD5+"\n"), 0o644)
	}

	// If the previous cache was built from the same md5 and the DB has rows, skip re-parse.
	if prev, _ := st.Meta("md5"); prev != "" && strings.EqualFold(prev, localMD5) && !force {
		// Even if md5 unchanged, mark fresh so next call short-circuits on TTL.
		if err := st.MarkUpdated(time.Now(), localMD5); err != nil {
			return err
		}
		fmt.Fprintln(log, "  feed unchanged (md5 match), keeping existing tables.")
		return nil
	}

	fmt.Fprintln(log, "  parsing CSV → SQLite…")
	if err := importZip(ctx, c.zipPath(), st, log); err != nil {
		return fmt.Errorf("import zip: %w", err)
	}
	if err := st.MarkUpdated(time.Now(), localMD5); err != nil {
		return err
	}
	fmt.Fprintln(log, "  done.")
	return nil
}

func fetchString(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: httpTO}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: httpTO}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// importZip streams every required CSV file from zipPath into the SQLite store
// inside a single transaction. The store is truncated first.
func importZip(ctx context.Context, zipPath string, st *store.Store, log io.Writer) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[strings.ToLower(filepath.Base(f.Name))] = f
	}

	required := []string{"stops.txt", "routes.txt", "trips.txt", "stop_times.txt"}
	for _, name := range required {
		if _, ok := files[name]; !ok {
			return fmt.Errorf("required file missing from zip: %s", name)
		}
	}

	if err := st.Truncate(ctx); err != nil {
		return err
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	type loader struct {
		filename string
		required bool
		fn       func(*csv.Reader, []string) error
	}

	// agency
	loadAgency := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"agency_id", "agency_name", "agency_url", "agency_timezone"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO agency(agency_id, agency_name, agency_url, agency_tz) VALUES(?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		return forEachRow(r, func(row []string) error {
			id := getField(row, idx, "agency_id")
			if id == "" {
				id = "default"
			}
			_, err := stmt.ExecContext(ctx, id,
				getField(row, idx, "agency_name"),
				getField(row, idx, "agency_url"),
				getField(row, idx, "agency_timezone"))
			return err
		})
	}

	// stops
	loadStops := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"stop_id", "stop_code", "stop_name", "stop_lat", "stop_lon", "location_type", "parent_station"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO stops(stop_id, stop_code, stop_name, stop_lat, stop_lon, location_type, parent_station) VALUES(?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		count := 0
		err = forEachRow(r, func(row []string) error {
			id := getField(row, idx, "stop_id")
			if id == "" {
				return nil
			}
			_, err := stmt.ExecContext(ctx, id,
				getField(row, idx, "stop_code"),
				getField(row, idx, "stop_name"),
				parseFloat(getField(row, idx, "stop_lat")),
				parseFloat(getField(row, idx, "stop_lon")),
				parseInt(getField(row, idx, "location_type")),
				getField(row, idx, "parent_station"))
			if err == nil {
				count++
			}
			return err
		})
		fmt.Fprintf(log, "    stops: %d\n", count)
		return err
	}

	// routes
	loadRoutes := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"route_id", "agency_id", "route_short_name", "route_long_name", "route_type"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO routes(route_id, agency_id, route_short_name, route_long_name, route_type) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		count := 0
		err = forEachRow(r, func(row []string) error {
			id := getField(row, idx, "route_id")
			if id == "" {
				return nil
			}
			_, err := stmt.ExecContext(ctx, id,
				getField(row, idx, "agency_id"),
				getField(row, idx, "route_short_name"),
				getField(row, idx, "route_long_name"),
				parseInt(getField(row, idx, "route_type")))
			if err == nil {
				count++
			}
			return err
		})
		fmt.Fprintf(log, "    routes: %d\n", count)
		return err
	}

	// trips
	loadTrips := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"trip_id", "route_id", "service_id", "trip_headsign", "direction_id"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO trips(trip_id, route_id, service_id, trip_headsign, direction_id) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		count := 0
		err = forEachRow(r, func(row []string) error {
			id := getField(row, idx, "trip_id")
			if id == "" {
				return nil
			}
			_, err := stmt.ExecContext(ctx, id,
				getField(row, idx, "route_id"),
				getField(row, idx, "service_id"),
				getField(row, idx, "trip_headsign"),
				parseInt(getField(row, idx, "direction_id")))
			if err == nil {
				count++
			}
			return err
		})
		fmt.Fprintf(log, "    trips: %d\n", count)
		return err
	}

	// stop_times — this is the big one (millions of rows).
	loadStopTimes := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"trip_id", "stop_id", "arrival_time", "departure_time", "stop_sequence"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO stop_times(trip_id, stop_id, arrival_time, departure_time, stop_sequence) VALUES(?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		count := 0
		err = forEachRow(r, func(row []string) error {
			tid := getField(row, idx, "trip_id")
			sid := getField(row, idx, "stop_id")
			if tid == "" || sid == "" {
				return nil
			}
			arr := parseHMS(getField(row, idx, "arrival_time"))
			dep := parseHMS(getField(row, idx, "departure_time"))
			if dep < 0 {
				dep = arr
			}
			if arr < 0 {
				arr = dep
			}
			seq := parseInt(getField(row, idx, "stop_sequence"))
			if _, err := stmt.ExecContext(ctx, tid, sid, arr, dep, seq); err != nil {
				return err
			}
			count++
			if count%200000 == 0 {
				fmt.Fprintf(log, "    stop_times: %d…\n", count)
			}
			return nil
		})
		fmt.Fprintf(log, "    stop_times: %d\n", count)
		return err
	}

	loadCalendar := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"service_id", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "start_date", "end_date"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO calendar(service_id, monday, tuesday, wednesday, thursday, friday, saturday, sunday, start_date, end_date) VALUES(?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		return forEachRow(r, func(row []string) error {
			sid := getField(row, idx, "service_id")
			if sid == "" {
				return nil
			}
			_, err := stmt.ExecContext(ctx, sid,
				parseInt(getField(row, idx, "monday")),
				parseInt(getField(row, idx, "tuesday")),
				parseInt(getField(row, idx, "wednesday")),
				parseInt(getField(row, idx, "thursday")),
				parseInt(getField(row, idx, "friday")),
				parseInt(getField(row, idx, "saturday")),
				parseInt(getField(row, idx, "sunday")),
				getField(row, idx, "start_date"),
				getField(row, idx, "end_date"))
			return err
		})
	}

	loadCalendarDates := func(r *csv.Reader, header []string) error {
		idx := mapIdx(header, []string{"service_id", "date", "exception_type"})
		stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO calendar_dates(service_id, date, exception_type) VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		return forEachRow(r, func(row []string) error {
			sid := getField(row, idx, "service_id")
			d := getField(row, idx, "date")
			if sid == "" || d == "" {
				return nil
			}
			_, err := stmt.ExecContext(ctx, sid, d, parseInt(getField(row, idx, "exception_type")))
			return err
		})
	}

	loaders := []loader{
		{"agency.txt", false, loadAgency},
		{"stops.txt", true, loadStops},
		{"routes.txt", true, loadRoutes},
		{"trips.txt", true, loadTrips},
		{"stop_times.txt", true, loadStopTimes},
		{"calendar.txt", false, loadCalendar},
		{"calendar_dates.txt", false, loadCalendarDates},
	}

	for _, ld := range loaders {
		zf, ok := files[ld.filename]
		if !ok {
			if ld.required {
				return fmt.Errorf("required file missing: %s", ld.filename)
			}
			continue
		}
		if err := runLoader(zf, ld.fn); err != nil {
			return fmt.Errorf("%s: %w", ld.filename, err)
		}
	}

	return tx.Commit()
}

// runLoader opens a single CSV inside the zip, reads the header, and invokes fn.
func runLoader(zf *zip.File, fn func(*csv.Reader, []string) error) error {
	f, err := zf.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err != nil {
		return err
	}
	for i := range header {
		header[i] = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(header[i]), "\ufeff"))
	}
	return fn(r, header)
}

// forEachRow reads rows from r and invokes fn for each, stopping on EOF or first error.
func forEachRow(r *csv.Reader, fn func([]string) error) error {
	for {
		row, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Tolerate malformed rows: keep going so a single bad line doesn't kill the import.
			if csvErr, ok := err.(*csv.ParseError); ok {
				_ = csvErr
				continue
			}
			return err
		}
		if err := fn(row); err != nil {
			return err
		}
	}
}

// mapIdx returns a name→column-index map for the columns we care about.
// Missing columns map to -1.
func mapIdx(header, wanted []string) map[string]int {
	out := make(map[string]int, len(wanted))
	for _, w := range wanted {
		out[w] = -1
		for i, h := range header {
			if h == w {
				out[w] = i
				break
			}
		}
	}
	return out
}

func getField(row []string, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseHMS converts a GTFS HH:MM:SS string (which may exceed 24h) into seconds.
// Returns -1 for empty or malformed input.
func parseHMS(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return -1
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return -1
	}
	return h*3600 + m*60 + sec
}
