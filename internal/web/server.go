// Package web serves the CircusNet dashboard: a live simulation with manual
// scenario controls and JSON APIs for every panel.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"circusnet/internal/fabric"
	"circusnet/internal/model"
	"circusnet/internal/sim"
)

//go:embed static/*
var staticFS embed.FS

// Server owns one live simulation and serialises access to it.
type Server struct {
	mu       sync.Mutex
	World    *sim.World
	Scenario string
	rng      *rand.Rand
	// LastVerify caches the latest verification summary.
	LastVerify *sim.VerifyReport
	// StressReport is an optional stress-test report loaded from disk.
	StressReport *sim.StressReport
}

// NewServer builds a live world from a config.
func NewServer(cfg sim.Config) *Server {
	topo := sim.GenerateTopology(cfg)
	w := sim.NewWorld(topo, sim.DefaultTimes())
	s := &Server{World: w, Scenario: "live", rng: rand.New(rand.NewSource(cfg.Seed ^ 0x1234))}
	w.RebuildAllFIBs()
	return s
}

// Handler returns the dashboard routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/switch/{id}", s.handleSwitch)
	mux.HandleFunc("GET /api/trace", s.handleTrace)
	mux.HandleFunc("POST /api/action", s.handleAction)
	mux.HandleFunc("GET /api/verify", s.handleVerify)
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return mux
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	snap := s.World.BuildSnapshot(s.Scenario)
	s.mu.Unlock()
	// The live world can be large; cap the per-switch detail so the payload
	// stays responsive. The dedicated switch endpoint returns everything.
	trimSnapshot(snap)
	writeJSON(w, snap)
}

// trimSnapshot keeps the overview payload bounded while leaving the selected
// switch's detail available through /api/switch/{id}.
func trimSnapshot(snap *sim.Snapshot) {
	for i := range snap.Switches {
		sw := &snap.Switches[i]
		sw.Routes = trimRoutes(sw.Routes, 120)
		if len(sw.Hosts) > 90 {
			sw.Hosts = sw.Hosts[:90]
		}
		if len(sw.Tunnels) > 60 {
			sw.Tunnels = sw.Tunnels[:60]
		}
	}
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	sw, ok := s.World.FindSwitch(id)
	if !ok {
		http.Error(w, "unknown switch", http.StatusNotFound)
		return
	}
	writeJSON(w, s.World.SwitchSnapshot(sw))
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	srcQ := r.URL.Query().Get("src")
	dstQ := r.URL.Query().Get("dst")
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok1 := s.World.FindDeviceByAddr(srcQ)
	dst, ok2 := s.World.FindDeviceByAddr(dstQ)
	if !ok1 || !ok2 {
		writeJSON(w, map[string]any{"note": "device not found"})
		return
	}
	writeJSON(w, s.World.TraceDevices(src.ID, dst.ID))
}

type actionRequest struct {
	Action string `json:"action"`
	Switch string `json:"switch"`
	Device int32  `json:"device"`
	Group  string `json:"group"`
	VN     uint32 `json:"vn"`
	From   string `json:"from"`
	To     string `json:"to"`
	Count  int    `json:"count"`
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var req actionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wld := s.World
	changed := false
	detail := ""
	switch req.Action {
	case "move":
		d := wld.Topo.Device(model.DeviceID(req.Device))
		if d != nil {
			changed = wld.MoveDevice(d, s.rng)
			detail = fmt.Sprintf("moved device %d", req.Device)
		}
	case "move-random":
		n := req.Count
		if n <= 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			for tries := 0; tries < 40; tries++ {
				d := wld.Topo.Devices[s.rng.Intn(len(wld.Topo.Devices))]
				if d.Kind != model.AttachSingle || d.External {
					continue
				}
				if wld.MoveDevice(d, s.rng) {
					changed = true
					break
				}
			}
		}
		detail = fmt.Sprintf("moved up to %d devices", n)
	case "link-down", "link-up":
		up := req.Action == "link-up"
		k := fabric.NewLinkKey(model.SwitchID(req.From), model.SwitchID(req.To))
		changed = wld.FlipLink(k, up)
		detail = req.Action + " " + req.From + "-" + req.To
	case "access-down", "access-up":
		up := req.Action == "access-up"
		changed = wld.SetAccess(model.GroupID(req.Group), model.SwitchID(req.Switch), up)
		detail = req.Action + " " + req.Group + "/" + req.Switch
	case "switch-down":
		changed = wld.SwitchDown(model.SwitchID(req.Switch))
		detail = "switch down " + req.Switch
	case "switch-up":
		changed = wld.SwitchUp(model.SwitchID(req.Switch))
		detail = "switch restart " + req.Switch
	case "vn-leave", "vn-join":
		join := req.Action == "vn-join"
		changed = wld.SetVNMembership(model.SwitchID(req.Switch), model.VNI(req.VN), join)
		detail = req.Action + " " + req.Switch
	case "verify":
		s.LastVerify = wld.VerifyAll(sim.VerifyOptions{Pairs: 16, Seed: time.Now().UnixNano() % 1000})
		writeJSON(w, map[string]any{"action": "verify", "report": s.LastVerify})
		return
	case "random":
		changed = randomChange(wld, s.rng)
		detail = "random change"
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	rounds := 0
	var report *sim.VerifyReport
	if changed {
		rounds = wld.Converge()
		report = wld.VerifyAll(sim.VerifyOptions{Pairs: 8, Seed: time.Now().UnixNano() % 1000})
		s.LastVerify = report
	}
	writeJSON(w, map[string]any{
		"changed": changed, "detail": detail, "rounds": rounds,
		"verify": report,
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rep := s.World.VerifyAll(sim.VerifyOptions{Pairs: 24, Seed: time.Now().UnixNano() % 1000})
	s.LastVerify = rep
	s.mu.Unlock()
	writeJSON(w, rep)
}

func randomChange(w *sim.World, rng *rand.Rand) bool {
	switch rng.Intn(5) {
	case 0, 1:
		for tries := 0; tries < 30; tries++ {
			d := w.Topo.Devices[rng.Intn(len(w.Topo.Devices))]
			if d.Kind == model.AttachSingle && !d.External && w.MoveDevice(d, rng) {
				return true
			}
		}
	case 2:
		l := w.Topo.Links[rng.Intn(len(w.Topo.Links))]
		return w.FlipLink(fabric.NewLinkKey(l.A, l.B), !w.LinkIsUp(l.A, l.B))
	case 3:
		for gid, g := range w.Topo.Groups {
			m := g.Members[rng.Intn(len(g.Members))]
			if w.AccessUp[gid][m] {
				return w.SetAccess(gid, m, false)
			}
		}
	case 4:
		for _, id := range w.Topo.SwitchOrder {
			if w.SwUp[id] {
				w.SwitchDown(id)
				return true
			}
		}
	}
	return false
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// trimRoutes caps the route list while keeping at least a few of every route
// type, so the overview still demonstrates all four families on every switch.
func trimRoutes(in []sim.RouteView, limit int) []sim.RouteView {
	if len(in) <= limit {
		return in
	}
	byType := map[string][]sim.RouteView{}
	var order []string
	for _, r := range in {
		k := string(r.Type)
		if _, ok := byType[k]; !ok {
			order = append(order, k)
		}
		byType[k] = append(byType[k], r)
	}
	perType := limit / len(order)
	if perType < 4 {
		perType = 4
	}
	out := make([]sim.RouteView, 0, limit)
	for _, k := range order {
		rs := byType[k]
		if len(rs) > perType {
			rs = rs[:perType]
		}
		out = append(out, rs...)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func parseDevice(q string) (model.DeviceID, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(q))
	if err != nil {
		return 0, false
	}
	return model.DeviceID(n), true
}

// LoadReport attaches a stress-test report file to the live server so the
// dashboard can show its statistics.
func (s *Server) LoadReport(path string) error {
	var rep sim.StressReport
	if err := readJSONFile(path, &rep); err != nil {
		return err
	}
	s.StressReport = &rep
	return nil
}
