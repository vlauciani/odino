// Command odino is a CLI for Rome public-transport arrivals, vehicle positions,
// and service alerts, backed by the Roma Mobilità GTFS open-data feeds.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	// Embed the IANA tz database into the binary so time.LoadLocation("Europe/Rome")
	// works on any host, including minimal systems (scratch/distroless containers,
	// stripped distros) that ship no system zoneinfo. Without this a prebuilt binary
	// could fail at startup on machines we don't control.
	_ "time/tzdata"

	"github.com/vlauciani/odino/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root := cli.NewRootCmd()
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
