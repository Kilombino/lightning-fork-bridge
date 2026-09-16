// Command bridged runs the swap bridge.
//
//	bridged -config /etc/bridged/config.json
//	bridged -config /etc/bridged/config.json -check
//
// -check loads the configuration, dials both nodes and opens the journal, then
// exits without quoting anything. That is the difference worth having: a
// configuration error found at deploy time costs a restart, and the same error
// found later costs whatever was in flight.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/paulscode/lightning-fork-bridge/bridged"
)

func main() {
	var (
		path  = flag.String("config", "", "path to the configuration file")
		check = flag.Bool("check", false,
			"load the configuration, connect, and exit without serving")
		debug = flag.Bool("debug", false, "log at debug level")
	)
	flag.Parse()

	if *path == "" {
		fmt.Fprintln(os.Stderr, "bridged: -config is required")
		flag.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))

	if err := run(*path, *check, log); err != nil {
		log.Error("bridged stopped", "err", err)
		// A configuration this daemon will not run on is the operator's to
		// fix, and is worth a different exit code from a node that went
		// away, so a supervisor can tell "will never start" from "try
		// again".
		if errors.Is(err, bridged.ErrConfig) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(path string, check bool, log *slog.Logger) error {
	cfg, err := bridged.LoadFile(path)
	if err != nil {
		return err
	}

	// Signals are caught before anything is opened, so an interrupt during
	// startup shuts down rather than being ignored until the surface is up.
	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := bridged.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer svc.Close()

	if check {
		log.Info("configuration is usable, both nodes answered, journal opened")

		return nil
	}

	return svc.Run(ctx)
}
