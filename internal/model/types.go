// Package model contains the shared, serialisable domain types for the
// CircusNet simulation: switches, devices, virtual networks, redundancy
// groups and underlay links.
package model

import (
	"fmt"
	"net/netip"
)

type SwitchID string
type VNI uint32
type DeviceID int32
type GroupID string

func (s SwitchID) String() string { return string(s) }
func (v VNI) String() string      { return fmt.Sprintf("%d", uint32(v)) }
func (g GroupID) String() string  { return string(g) }

// AttachmentKind says how a device is connected to the fabric.
type AttachmentKind string

const (
	AttachSingle AttachmentKind = "single" // access port on one switch
	AttachGroup  AttachmentKind = "group"  // dual-homed through a redundancy group
)

// VirtualNetwork is an L2/L3 virtual network (an EVPN EVI/VNI).
type VirtualNetwork struct {
	VNI              VNI            `json:"vni"`
	Name             string         `json:"name"`
	Subnet           netip.Prefix   `json:"subnet"`
	Gateway          netip.Addr     `json:"gateway"` // anycast: identical on every switch
	ExternalPrefixes []netip.Prefix `json:"externalPrefixes"`
	ExternalOwners   []SwitchID     `json:"externalOwners,omitempty"`
}

// SwitchInfo is the static identity and placement of a switch.
type SwitchInfo struct {
	ID       SwitchID   `json:"id"`
	Tent     int        `json:"tent"`
	Loopback netip.Addr `json:"loopback"`
	VNs      []VNI      `json:"vns"`
}

// Device is an end host. A device can move; the mutable attachment fields
// always describe its current physical location.
type Device struct {
	ID           DeviceID       `json:"id"`
	MAC          string         `json:"mac"`
	IP           netip.Addr     `json:"ip"`
	VN           VNI            `json:"vni"`
	Kind         AttachmentKind `json:"kind"`
	Switch       SwitchID       `json:"switch,omitempty"` // single-homed
	Port         string         `json:"port"`
	Group        GroupID        `json:"group,omitempty"` // dual-homed
	GroupMembers []SwitchID     `json:"groupMembers,omitempty"`
	// External marks a simulated host on an external segment. Its IP is in
	// the VN's external prefix rather than the VN subnet. ExternalKnown
	// means the border also has a /32 host route for it, which must beat the
	// covering prefix route.
	External      bool `json:"external,omitempty"`
	ExternalKnown bool `json:"externalKnown,omitempty"`
}

// RedundancyGroup is an ESI-style multi-homing group. Members are the two
// switches the group is dual-homed to. An optional AccessID names a simulated
// shared segment between the members (an access switch); routing over that
// segment is still normal routed forwarding.
type RedundancyGroup struct {
	ID       GroupID    `json:"id"`
	VN       VNI        `json:"vn"`
	Members  []SwitchID `json:"members"`
	AccessID SwitchID   `json:"access,omitempty"`
}

// SwitchLinks are the physical (underlay) links of a switch. Tunnels ride on
// this topology.
type Link struct {
	A SwitchID `json:"a"`
	B SwitchID `json:"b"`
}

// HasMember reports whether the switch is a member of the redundancy group.
func (g *RedundancyGroup) HasMember(sw SwitchID) bool {
	for _, m := range g.Members {
		if m == sw {
			return true
		}
	}
	return false
}

// InVirtualNetwork reports whether a switch participates in a VN.
func (s SwitchInfo) InVirtualNetwork(vni VNI) bool {
	for _, v := range s.VNs {
		if v == vni {
			return true
		}
	}
	return false
}
