package fabric

import (
	"sort"

	"circusnet/internal/model"
)

type LinkKey struct{ A, B string }

func NewLinkKey(a, b model.SwitchID) LinkKey {
	if a > b {
		a, b = b, a
	}
	return LinkKey{string(a), string(b)}
}

// Underlay is the physical switch topology that carries the overlay tunnels.
// Shortest paths are computed with BFS over up links.
type Underlay struct {
	nodes map[model.SwitchID]bool
	links map[LinkKey]bool
	adj   map[model.SwitchID][]model.SwitchID
	all   map[model.SwitchID]map[model.SwitchID][]model.SwitchID
	dist  map[model.SwitchID]map[model.SwitchID]int
	nup   map[model.SwitchID]bool
	dirty bool
}

func NewUnderlay() *Underlay {
	return &Underlay{
		nodes: map[model.SwitchID]bool{},
		links: map[LinkKey]bool{},
		adj:   map[model.SwitchID][]model.SwitchID{},
		nup:   map[model.SwitchID]bool{},
		dirty: true,
	}
}

// SetNodeUp removes or restores a switch's links. A down switch is not a
// transit or destination node.
func (u *Underlay) SetNodeUp(id model.SwitchID, up bool) {
	if _, known := u.nup[id]; known && u.nup[id] == up {
		return
	}
	u.nup[id] = up
	u.dirty = true
}

func (u *Underlay) nodeUp(id model.SwitchID) bool {
	if _, known := u.nup[id]; !known {
		return true
	}
	return u.nup[id]
}

func (u *Underlay) AddNode(id model.SwitchID) {
	u.nodes[id] = true
	u.dirty = true
}

func (u *Underlay) SetLink(a, b model.SwitchID, up bool) {
	k := NewLinkKey(a, b)
	u.links[k] = up
	u.dirty = true
}

func (u *Underlay) LinkUp(a, b model.SwitchID) bool { return u.links[NewLinkKey(a, b)] }

func (u *Underlay) Nodes() []model.SwitchID {
	out := make([]model.SwitchID, 0, len(u.nodes))
	for n := range u.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (u *Underlay) rebuild() {
	if !u.dirty {
		return
	}
	u.adj = map[model.SwitchID][]model.SwitchID{}
	for k, up := range u.links {
		if !up {
			continue
		}
		a, b := model.SwitchID(k.A), model.SwitchID(k.B)
		if !u.nodeUp(a) || !u.nodeUp(b) {
			continue
		}
		u.adj[a] = append(u.adj[a], b)
		u.adj[b] = append(u.adj[b], a)
	}
	for n := range u.adj {
		sort.Slice(u.adj[n], func(i, j int) bool { return u.adj[n][i] < u.adj[n][j] })
	}
	u.all = map[model.SwitchID]map[model.SwitchID][]model.SwitchID{}
	u.dist = map[model.SwitchID]map[model.SwitchID]int{}
	for src := range u.nodes {
		if !u.nodeUp(src) {
			continue
		}
		dist := map[model.SwitchID]int{src: 0}
		prev := map[model.SwitchID]model.SwitchID{}
		queue := []model.SwitchID{src}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, nxt := range u.adj[cur] { // sorted adjacency: deterministic
				if _, seen := dist[nxt]; seen {
					continue
				}
				dist[nxt] = dist[cur] + 1
				prev[nxt] = cur
				queue = append(queue, nxt)
			}
		}
		u.dist[src] = dist
		paths := map[model.SwitchID][]model.SwitchID{}
		for dst := range dist {
			var rev []model.SwitchID
			for at := dst; ; at = prev[at] {
				rev = append(rev, at)
				if at == src {
					break
				}
			}
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			paths[dst] = rev
		}
		u.all[src] = paths
	}
	u.dirty = false
}

// Path returns the physical switch sequence from src to dst, inclusive. A
// single node is returned as a one-element path. Nil means unreachable.
func (u *Underlay) Path(src, dst model.SwitchID) []model.SwitchID {
	u.rebuild()
	if src == dst {
		if !u.nodeUp(src) {
			return nil
		}
		return []model.SwitchID{src}
	}
	p, ok := u.all[src][dst]
	if !ok || p == nil {
		return nil
	}
	return append([]model.SwitchID(nil), p...)
}

func (u *Underlay) Reachable(src, dst model.SwitchID) bool {
	u.rebuild()
	if src == dst {
		return true
	}
	_, ok := u.all[src][dst]
	return ok
}

func (u *Underlay) Links() []model.Link {
	out := make([]model.Link, 0, len(u.links))
	for k, up := range u.links {
		if up {
			out = append(out, model.Link{A: model.SwitchID(k.A), B: model.SwitchID(k.B)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}
