package sim

import (
	"circusnet/internal/model"
)

// indexes holds derived lookup tables rebuilt after any topology mutation.
type indexes struct {
	devicesAt    map[model.SwitchID][]*model.Device // primary attachment
	groupDevices map[model.GroupID][]*model.Device
	groupTags    map[model.SwitchID]map[model.VNI][]model.GroupID
	groupsOfVN   map[model.VNI][]model.GroupID
	vnDevices    map[model.VNI][]*model.Device
	externalIP   map[model.VNI]map[string]*model.Device // device MAC -> device
}

func (w *World) reindex() {
	idx := &indexes{
		devicesAt:    map[model.SwitchID][]*model.Device{},
		groupDevices: map[model.GroupID][]*model.Device{},
		groupTags:    map[model.SwitchID]map[model.VNI][]model.GroupID{},
		groupsOfVN:   map[model.VNI][]model.GroupID{},
		vnDevices:    map[model.VNI][]*model.Device{},
		externalIP:   map[model.VNI]map[string]*model.Device{},
	}
	for _, d := range w.Topo.Devices {
		idx.vnDevices[d.VN] = append(idx.vnDevices[d.VN], d)
		w.macIndex[d.MAC] = d
		if d.Kind == model.AttachGroup {
			idx.groupDevices[d.Group] = append(idx.groupDevices[d.Group], d)
		} else {
			idx.devicesAt[d.Switch] = append(idx.devicesAt[d.Switch], d)
		}
		if d.External {
			if idx.externalIP[d.VN] == nil {
				idx.externalIP[d.VN] = map[string]*model.Device{}
			}
			idx.externalIP[d.VN][d.MAC] = d
		}
	}
	for gid, g := range w.Topo.Groups {
		idx.groupsOfVN[g.VN] = append(idx.groupsOfVN[g.VN], gid)
		for _, m := range g.Members {
			if idx.groupTags[m] == nil {
				idx.groupTags[m] = map[model.VNI][]model.GroupID{}
			}
			idx.groupTags[m][g.VN] = appendUniqueGroup(idx.groupTags[m][g.VN], gid)
		}
	}
	w.idx = idx
}

func appendUniqueGroup(in []model.GroupID, gid model.GroupID) []model.GroupID {
	for _, g := range in {
		if g == gid {
			return in
		}
	}
	return append(in, gid)
}

// DeviceAt reports the authoritative attachment of a device: which switches
// can reach it right now.
func (w *World) DeviceAt(d *model.Device) []model.SwitchID {
	switch d.Kind {
	case model.AttachSingle:
		if d.External {
			// External hosts live on a segment shared by both border
			// switches; the second border advertises host routes for them.
			for _, vn := range w.Topo.VNs {
				if vn.VNI != d.VN {
					continue
				}
				return append([]model.SwitchID(nil), vn.ExternalOwners...)
			}
			return nil
		}
		if !w.SwUp[d.Switch] {
			return nil
		}
		return []model.SwitchID{d.Switch}
	case model.AttachGroup:
		var out []model.SwitchID
		if w.AccessUp[d.Group][d.Switch] && w.SwUp[d.Switch] {
			out = append(out, d.Switch)
		}
		for _, m := range d.GroupMembers {
			if m == d.Switch || !w.AccessUp[d.Group][m] || !w.SwUp[m] {
				continue
			}
			out = append(out, m)
		}
		return out
	}
	return nil
}

// LocalDevices returns devices the switch delivers locally.
func (w *World) LocalDevices(sw model.SwitchID) []*model.Device {
	var out []*model.Device
	for _, d := range w.idx.devicesAt[sw] {
		if d.External {
			continue
		}
		if d.Kind == model.AttachGroup && !w.AccessUp[d.Group][sw] {
			continue
		}
		out = append(out, d)
	}
	// Group devices are local on their primary member only.
	for _, d := range w.Topo.Devices {
		if d.Kind == model.AttachGroup && d.Switch == sw && w.AccessUp[d.Group][sw] {
			out = append(out, d)
		}
	}
	return out
}
