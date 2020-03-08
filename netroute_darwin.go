// Copyright 2012 Google, Inc. All rights reserved.
//
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file in the root of the source
// tree.

// +build darwin dragonfly freebsd netbsd openbsd

// This is a copy of
// https://github.com/google/gopacket/blob/master/routing/routing.go
// but with RIB parsing following the route format described in
// https://github.com/freebsd/freebsd/blob/master/sys/net/route.h
package netroute

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"syscall"

	"github.com/google/gopacket/routing"
	"golang.org/x/net/route"
)

// Pulled from http://man7.org/linux/man-pages/man7/rtnetlink.7.html
// See the section on RTM_NEWROUTE, specifically 'struct rtmsg'.
type routeInfoInMemory struct {
	Family byte
	DstLen byte
	SrcLen byte
	TOS    byte

	Table    byte
	Protocol byte
	Scope    byte
	Type     byte

	Flags uint32
}

// rtInfo contains information on a single route.
type rtInfo struct {
	Src, Dst         *net.IPNet
	Gateway, PrefSrc net.IP
	// We currently ignore the InputIface.
	InputIface, OutputIface uint32
	Priority                uint32
}

// routeSlice implements sort.Interface to sort routes by Priority.
type routeSlice []*rtInfo

func (r routeSlice) Len() int {
	return len(r)
}
func (r routeSlice) Less(i, j int) bool {
	return r[i].Priority < r[j].Priority
}
func (r routeSlice) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

type router struct {
	ifaces []net.Interface
	addrs  []ipAddrs
	v4, v6 routeSlice
}

func (r *router) String() string {
	strs := []string{"ROUTER", "--- V4 ---"}
	for _, route := range r.v4 {
		strs = append(strs, fmt.Sprintf("%+v", *route))
	}
	strs = append(strs, "--- V6 ---")
	for _, route := range r.v6 {
		strs = append(strs, fmt.Sprintf("%+v", *route))
	}
	return strings.Join(strs, "\n")
}

type ipAddrs struct {
	v4, v6 net.IP
}

func (r *router) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return r.RouteWithSrc(nil, nil, dst)
}

func (r *router) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	var ifaceIndex int
	switch {
	case dst.To4() != nil:
		ifaceIndex, gateway, preferredSrc, err = r.route(r.v4, input, src, dst)
	case dst.To16() != nil:
		ifaceIndex, gateway, preferredSrc, err = r.route(r.v6, input, src, dst)
	default:
		err = errors.New("IP is not valid as IPv4 or IPv6")
		return
	}
	if err != nil {
		return
	}

	// Interfaces are 1-indexed, but we store them in a 0-indexed array.
	ifaceIndex--

	iface = &r.ifaces[ifaceIndex]
	if preferredSrc == nil {
		switch {
		case dst.To4() != nil:
			preferredSrc = r.addrs[ifaceIndex].v4
		case dst.To16() != nil:
			preferredSrc = r.addrs[ifaceIndex].v6
		}
	}
	return
}

func (r *router) route(routes routeSlice, input net.HardwareAddr, src, dst net.IP) (iface int, gateway, preferredSrc net.IP, err error) {
	var inputIndex uint32
	if input != nil {
		for i, iface := range r.ifaces {
			if bytes.Equal(input, iface.HardwareAddr) {
				// Convert from zero- to one-indexed.
				inputIndex = uint32(i + 1)
				break
			}
		}
	}
	var mostSpecificRt *rtInfo
	for _, rt := range routes {
		if rt.InputIface != 0 && rt.InputIface != inputIndex {
			continue
		}
		if src != nil && rt.Src != nil && !rt.Src.Contains(src) {
			continue
		}
		if rt.Dst != nil && !rt.Dst.Contains(dst) {
			continue
		}
		if mostSpecificRt != nil {
			candSpec, _ := rt.Dst.Mask.Size()
			if curSpec, _ := mostSpecificRt.Dst.Mask.Size(); candSpec < curSpec {
				continue
			}
		}
		mostSpecificRt = rt
	}
	if mostSpecificRt != nil {
		return int(mostSpecificRt.OutputIface), mostSpecificRt.Gateway, mostSpecificRt.PrefSrc, nil
	}
	err = fmt.Errorf("no route found for %v", dst)
	return
}

// Begin modifications

func toIPAddr(a route.Addr) (net.IP, error) {
	switch t := a.(type) {
	case *route.Inet4Addr:
		ip := net.IPv4(t.IP[0], t.IP[1], t.IP[2], t.IP[3])
		return ip, nil
	case *route.Inet6Addr:
		ip := make(net.IP, net.IPv6len)
		copy(ip, t.IP[:])
		return ip, nil
	default:
		return net.IP{}, fmt.Errorf("unknown family: %v", t)
	}
}

// selected BSD Route flags.
const (
	RTF_UP        = 0x1
	RTF_GATEWAY   = 0x2
	RTF_HOST      = 0x4
	RTF_REJECT    = 0x8
	RTF_DYNAMIC   = 0x10
	RTF_MODIFIED  = 0x20
	RTF_STATIC    = 0x800
	RTF_BLACKHOLE = 0x1000
	RTF_LOCAL     = 0x200000
	RTF_BROADCAST = 0x400000
	RTF_MULTICAST = 0x800000
)

func New() (routing.Router, error) {
	rtr := &router{}
	tab, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, tab)
	if err != nil {
		return nil, err
	}
	var ipn *net.IPNet
	for _, msg := range msgs {
		m := msg.(*route.RouteMessage)
		routeInfo := new(rtInfo)

		if m.Version < 3 || m.Version > 5 {
			return nil, fmt.Errorf("Unexpected RIB message version: %d", m.Version)
		}
		if m.Type != 4 /* RTM_GET */ {
			return nil, fmt.Errorf("Unexpected RIB message type: %d", m.Type)
		}

		if m.Flags&RTF_UP == 0 ||
			m.Flags&(RTF_REJECT|RTF_BLACKHOLE) != 0 {
			continue
		}
		if m.Err != nil {
			continue
		}

		dst, err := toIPAddr(m.Addrs[0])
		if err == nil {
			mask, _ := toIPAddr(m.Addrs[2])
			if mask == nil {
				mask = net.IP(net.CIDRMask(0, 8*len(dst)))
			}
			ipn = &net.IPNet{IP: dst, Mask: net.IPMask(mask)}
			if m.Flags&RTF_HOST != 0 {
				ipn.Mask = net.CIDRMask(8*len(ipn.IP), 8*len(ipn.IP))
			}
			routeInfo.Dst = ipn
		} else {
			return nil, fmt.Errorf("Unexpected RIB destination: %v", err)
		}

		if m.Flags&RTF_GATEWAY != 0 {
			if gw, err := toIPAddr(m.Addrs[1]); err == nil {
				routeInfo.Gateway = gw
			}
		}
		if src, err := toIPAddr(m.Addrs[5]); err == nil {
			ipn = &net.IPNet{IP: src, Mask: net.CIDRMask(8*len(src), 8*len(src))}
			routeInfo.Src = ipn
			routeInfo.PrefSrc = src
			if m.Flags&0x2 != 0 /* RTF_GATEWAY */ {
				routeInfo.Src.Mask = net.CIDRMask(0, 8*len(routeInfo.Src.IP))
			}
		}
		routeInfo.OutputIface = uint32(m.Index)

		switch m.Addrs[0].(type) {
		case *route.Inet4Addr:
			rtr.v4 = append(rtr.v4, routeInfo)
		case *route.Inet6Addr:
			rtr.v6 = append(rtr.v6, routeInfo)
		}
	}
	sort.Sort(rtr.v4)
	sort.Sort(rtr.v6)
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i, iface := range ifaces {
		if i != iface.Index-1 {
			return nil, fmt.Errorf("out of order iface %d = %v", i, iface)
		}
		rtr.ifaces = append(rtr.ifaces, iface)
		var addrs ipAddrs
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range ifaceAddrs {
			if inet, ok := addr.(*net.IPNet); ok {
				// Go has a nasty habit of giving you IPv4s as ::ffff:1.2.3.4 instead of 1.2.3.4.
				// We want to use mapped v4 addresses as v4 preferred addresses, never as v6
				// preferred addresses.
				if v4 := inet.IP.To4(); v4 != nil {
					if addrs.v4 == nil {
						addrs.v4 = v4
					}
				} else if addrs.v6 == nil {
					addrs.v6 = inet.IP
				}
			}
		}
		rtr.addrs = append(rtr.addrs, addrs)
	}
	return rtr, nil
}
