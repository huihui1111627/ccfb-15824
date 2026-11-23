package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"circusnet/internal/sim"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	cfg := sim.DefaultConfig()
	cfg.Seed = 7
	cfg.Tents = 5
	cfg.Devices = 240
	return NewServer(cfg)
}

func TestSnapshotSections(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var snap sim.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Switches) == 0 || len(snap.Groups) == 0 || len(snap.VNs) == 0 {
		t.Fatalf("empty snapshot: %d switches, %d groups, %d vns", len(snap.Switches), len(snap.Groups), len(snap.VNs))
	}
	sawHost, sawGroup, sawMember, sawPrefix, sawTunnel, sawTimeline := false, false, false, false, false, false
	for _, sw := range snap.Switches {
		for _, h := range sw.Hosts {
			if h.MAC != "" && h.IP != "" {
				sawHost = true
			}
		}
		for _, r := range sw.Routes {
			switch string(r.Type) {
			case "group":
				sawGroup = true
			case "member":
				sawMember = true
			case "prefix":
				sawPrefix = true
			}
		}
		if len(sw.Tunnels) > 0 {
			sawTunnel = true
		}
	}
	if len(snap.Timeline) > 0 {
		sawTimeline = true
	}
	if !sawHost || !sawGroup || !sawMember || !sawPrefix || !sawTunnel || !sawTimeline {
		t.Fatalf("missing sections: host=%v group=%v member=%v prefix=%v tunnel=%v timeline=%v",
			sawHost, sawGroup, sawMember, sawPrefix, sawTunnel, sawTimeline)
	}
}

func TestActionsAndTrace(t *testing.T) {
	s := testServer(t)
	h := s.Handler()
	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("action status %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := post(`{"action":"move-random","count":3}`); out["changed"] != true {
		t.Fatalf("move changed=%v", out["changed"])
	}
	if out := post(`{"action":"switch-down","switch":"sw003"}`); out["changed"] != true {
		t.Fatalf("switch-down changed=%v", out["changed"])
	}
	if out := post(`{"action":"switch-up","switch":"sw003"}`); out["changed"] != true {
		t.Fatalf("switch-up changed=%v", out["changed"])
	}
	if out := post(`{"action":"random"}`); out["changed"] != true {
		t.Fatalf("random changed=%v", out["changed"])
	}
	// Trace between two real devices.
	w := s.World
	d0, d1 := w.Topo.Devices[0], w.Topo.Devices[1]
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/trace?src="+d0.MAC+"&dst="+d1.IP.String(), nil))
	var q sim.QueryResult
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatalf("trace decode: %v", err)
	}
	if q.Result == nil || !q.Result.OK {
		t.Fatalf("trace failed: %+v note=%s", q.Result, q.Note)
	}
}

func TestIndexPageServed(t *testing.T) {
	s := testServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"地址表", "路由", "隧道", "时间线", "转发路径"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing section %q", want)
		}
	}
}
