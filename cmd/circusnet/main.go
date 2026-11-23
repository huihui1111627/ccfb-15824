// Command circusnet serves the CircusNet dashboard for a live simulated
// circus network.
//
//	circusnet -addr :8080 -seed 20261124 -devices 4000 -tents 32
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"circusnet/internal/sim"
	"circusnet/internal/web"
)

func main() {
	var (
		addr    = flag.String("addr", "127.0.0.1:8080", "listen address")
		seed    = flag.Int64("seed", 20261124, "fixed random seed")
		tents   = flag.Int("tents", 24, "number of tents")
		devices = flag.Int("devices", 3000, "number of devices")
		vns     = flag.Int("vns", 6, "number of virtual networks")
		report  = flag.String("report", "", "optional stress report JSON to load into the sidebar")
	)
	flag.Parse()

	cfg := sim.DefaultConfig()
	cfg.Seed = *seed
	cfg.Tents = *tents
	cfg.Devices = *devices
	cfg.VNs = *vns
	cfg.FullMeshVNs = true

	start := time.Now()
	srv := web.NewServer(cfg)
	if *report != "" {
		_ = srv.LoadReport(*report)
	}
	log.Printf("circusnet: %d tents, %d switches, %d devices, %d VNs, initial convergence %d rounds (%s)",
		cfg.Tents, len(srv.World.Topo.SwitchOrder), len(srv.World.Topo.Devices), len(srv.World.Topo.VNs),
		srv.World.TotalRounds, time.Since(start).Round(time.Millisecond))
	log.Printf("listening on http://%s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

var _ = fmt.Sprintf
