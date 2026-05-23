// Package cli wires the Cobra command tree for odino.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/gtfs"
	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/realtime"
	"gitlab.rm.ingv.it/valentino.lauciani/odino/internal/store"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// rootFlags are the persistent flags shared across subcommands.
type rootFlags struct {
	asJSON bool
}

// app bundles the shared dependencies a command needs.
type app struct {
	cache *gtfs.Cache
	store *store.Store
	rt    *realtime.Fetcher
	loc   *time.Location
	flags *rootFlags
	out   io.Writer
	err   io.Writer
}

// Execute is the entry point: returns the configured root command.
func Execute() error {
	cmd := NewRootCmd()
	return cmd.Execute()
}

// NewRootCmd builds the Cobra root.
func NewRootCmd() *cobra.Command {
	flags := &rootFlags{}

	root := &cobra.Command{
		Use:           "odino",
		Short:         "Realtime bus and tram arrivals for Rome (Roma Mobilità GTFS feeds).",
		Long:          "odino is a command-line client for the Roma Mobilità open data feeds (GTFS static + GTFS-Realtime). It reports next arrivals at a stop, live vehicle positions, service alerts, and lets you search stops and routes.",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       Version,
	}
	root.PersistentFlags().BoolVar(&flags.asJSON, "json", false, "Emit machine-readable JSON instead of a table.")

	root.AddCommand(
		newUpdateCmd(flags),
		newArrivalsCmd(flags),
		newVehiclesCmd(flags),
		newAlertsCmd(flags),
		newStopsCmd(flags),
		newRoutesCmd(flags),
		newMCPCmd(flags),
	)

	return root
}

// buildApp opens the cache, ensures freshness, and returns the wired dependencies.
// Pass force=true to skip the TTL check (used by `odino update`).
func buildApp(ctx context.Context, flags *rootFlags, force bool, out, errW io.Writer) (*app, error) {
	cache, err := gtfs.NewCache()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cache.DBPath())
	if err != nil {
		return nil, err
	}
	if _, err := cache.EnsureFresh(ctx, st, 24*time.Hour, force, errW); err != nil {
		_ = st.Close()
		return nil, err
	}
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		// Fall back to system local if tzdata is missing (rare; modernc builds bundle it).
		loc = time.Local
	}
	return &app{
		cache: cache,
		store: st,
		rt:    realtime.NewFetcher(),
		loc:   loc,
		flags: flags,
		out:   out,
		err:   errW,
	}, nil
}

// close releases the store.
func (a *app) close() { _ = a.store.Close() }

// printErr writes msg to stderr-with-prefix.
func (a *app) printErr(format string, args ...any) {
	fmt.Fprintf(a.err, "warning: "+strings.TrimSuffix(format, "\n")+"\n", args...)
}

// stdIO returns the writers; default to os.Stdout/Stderr if unset.
func stdIO(cmd *cobra.Command) (io.Writer, io.Writer) {
	out := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()
	if out == nil {
		out = os.Stdout
	}
	if errW == nil {
		errW = os.Stderr
	}
	return out, errW
}
