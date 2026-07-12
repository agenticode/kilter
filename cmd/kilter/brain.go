package main

import (
	"flag"
	"os"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/store"
)

func runBrain(args []string) error {
	fs := flag.NewFlagSet("brain", flag.ExitOnError)
	listen := fs.String("listen", envOr("KILTER_LISTEN", ":8180"), "listen address")
	dbPath := fs.String("db", envOr("KILTER_DB", "kilter.db"), "bbolt database path ('' = memory only)")
	token := fs.String("token", os.Getenv("KILTER_TOKEN"), "require this bearer token on /api routes")
	catalogPath := fs.String("catalog", "", "custom pricing catalog JSON (default: embedded baseline)")
	forecasterURL := fs.String("forecaster-url", os.Getenv("KILTER_FORECASTER_URL"), "external time-series model server (Chronos/TimesFM wrapper); built-in models are default+fallback")
	fs.Parse(args)

	catalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	var st *store.Store
	if *dbPath != "" {
		st, err = store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
	}
	brain, err := api.NewBrain(api.BrainConfig{Token: *token, ForecasterURL: *forecasterURL}, catalog, st)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	return brain.Serve(ctx, *listen)
}
