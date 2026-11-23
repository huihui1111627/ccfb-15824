package fabric

import (
	"net/netip"
	"testing"

	"circusnet/internal/model"
)

func TestRDAndRTAreDerived(t *testing.T) {
	lb := netip.MustParseAddr("10.255.0.7")
	if got, want := RD(lb, 10000), "10.255.0.7:10000"; got != want {
		t.Fatalf("RD=%s want %s", got, want)
	}
	rts := RTs("sw007", 10000)
	if rts[0] != "target:10000" {
		t.Fatalf("VN RT=%s", rts[0])
	}
	if rts[1] != "target:sw007:10000" {
		t.Fatalf("origin RT=%s", rts[1])
	}
}

func TestImportPolicyUsesRouteTarget(t *testing.T) {
	p := NewImportPolicy([]model.VNI{10000, 10200})
	in := NewHostRoute(model.SwitchInfo{ID: "sw1", Loopback: netip.MustParseAddr("10.0.0.1")},
		10000, "02:00:00:00:00:01", netip.MustParseAddr("10.1.0.2"), 1, 1)
	if !p.Accept(in) {
		t.Fatal("local VN route rejected")
	}
	// Same VN number but a forged RT that does not name a local target.
	bad := in
	bad.RTs = []string{"target:99999"}
	if p.Accept(bad) {
		t.Fatal("route with non-matching RT accepted")
	}
	other := NewHostRoute(model.SwitchInfo{ID: "sw1", Loopback: netip.MustParseAddr("10.0.0.1")},
		10300, "02:00:00:00:00:02", netip.MustParseAddr("10.2.0.2"), 1, 1)
	if p.Accept(other) {
		t.Fatal("route for an unconfigured VN accepted")
	}
}

func TestParseVNRT(t *testing.T) {
	cases := map[string]model.VNI{"target:10000": 10000, "target:0": 0}
	for in, want := range cases {
		got, ok := ParseVNRT(in)
		if !ok || got != want {
			t.Fatalf("%s -> %d,%v want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"target:sw1:10000", "10000", "target:x", ""} {
		if _, ok := ParseVNRT(in); ok {
			t.Fatalf("%q parsed as a VN target", in)
		}
	}
}

func TestUnderlayPathRespectsNodeState(t *testing.T) {
	u := NewUnderlay()
	for _, id := range []model.SwitchID{"a", "b", "c"} {
		u.AddNode(id)
	}
	u.SetLink("a", "b", true)
	u.SetLink("b", "c", true)
	if p := u.Path("a", "c"); len(p) != 3 {
		t.Fatalf("path %v", p)
	}
	u.SetNodeUp("b", false)
	if p := u.Path("a", "c"); p != nil {
		t.Fatalf("path through down node: %v", p)
	}
	u.SetNodeUp("b", true)
	u.SetLink("a", "b", false)
	if p := u.Path("a", "c"); p != nil {
		t.Fatalf("path over down link: %v", p)
	}
}

func TestHostRoutePrefersNewestAndMoreSpecific(t *testing.T) {
	// A newer MAC mobility sequence wins between origins.
	old := &HostFIB{Origin: "sw1", Seq: 1, Type: RouteHost}
	fresh := &HostFIB{Origin: "sw2", Seq: 2, Type: RouteHost}
	if !PreferHost(fresh, old) || PreferHost(old, fresh) {
		t.Fatal("sequence preference wrong")
	}
	// A host route beats a group entry at the same sequence.
	group := &HostFIB{Origin: "sw1", Seq: 2, Type: RouteGroup}
	if !PreferHost(fresh, group) {
		t.Fatal("host route should beat a group entry")
	}
	// Between two group members the lower origin wins deterministically.
	m1 := &HostFIB{Origin: "sw1", Seq: 2, Type: RouteGroup}
	m2 := &HostFIB{Origin: "sw2", Seq: 2, Type: RouteGroup}
	if !PreferHost(m1, m2) || PreferHost(m2, m1) {
		t.Fatal("group member tie-break not deterministic")
	}
	// Within one origin, a higher epoch wins even at the same sequence.
	e1 := &HostFIB{Origin: "sw1", Epoch: 1, Seq: 1, Type: RouteHost}
	e2 := &HostFIB{Origin: "sw1", Epoch: 2, Seq: 1, Type: RouteHost}
	if !PreferHost(e2, e1) {
		t.Fatal("epoch preference wrong")
	}
}

func TestVNFIBLookupHostBeatsPrefix(t *testing.T) {
	f := NewVNFIB(10000)
	f.SetPrefix(PrefixFIB{Prefix: netip.MustParsePrefix("172.16.0.0/16"), Origin: "swborder"})
	ip := netip.MustParseAddr("172.16.0.9")
	res, ok := f.Lookup(ip)
	if !ok || res.Type != RoutePrefix {
		t.Fatalf("prefix lookup failed: %+v", res)
	}
	f.InstallHost(&HostFIB{MAC: "02:00:00:00:00:09", IP: ip, Type: RouteHost,
		Origin: "swowner", Egress: Egress{Switch: "swowner", NextHop: "swowner"}})
	res, ok = f.Lookup(ip)
	if !ok || res.Type != RouteHost || res.Origin != "swowner" {
		t.Fatalf("host route did not beat prefix: %+v", res)
	}
}
