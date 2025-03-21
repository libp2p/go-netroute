// Copyright 2012 Google, Inc. All rights reserved.
//
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file in the root of the source
// tree.

//go:build darwin || dragonfly || freebsd || netbsd || openbsd

// This is a BSD import for the routing structure initially found in
// https://github.com/google/gopacket/blob/master/routing/routing.go
// RIB parsing follows the BSD route format described in
// https://github.com/freebsd/freebsd/blob/master/sys/net/route.h
package netroute

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/google/gopacket/routing"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

const (
	RTF_IFSCOPE = 0x1000000
)

type bsdRouter struct{}

func toIPAddr(a route.Addr) net.IP {
	switch t := a.(type) {
	case *route.Inet4Addr:
		return t.IP[:]
	case *route.Inet6Addr:
		return t.IP[:]
	default:
		return nil
	}
}

// toRouteAddr converts the net.IP to route.Addr .
// If ip is not an IPv4 address, toRouteAddr returns nil.
func toRouteAddr(ip net.IP) route.Addr {
	if len(ip) == 0 {
		return nil
	}

	if len(ip) != net.IPv4len && len(ip) != net.IPv6len {
		return nil
	}
	if p4 := ip.To4(); len(p4) == net.IPv4len {
		return &route.Inet4Addr{IP: [4]byte(p4)}
	}
	return &route.Inet6Addr{IP: [16]byte(ip)}
}

// ipToIfIndex takes an IP and returns index of the interface with the given IP assigned if any
func ipToIfIndex(ip net.IP) (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return -1, fmt.Errorf("failed to get interfaces: %s", err)
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			inet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if inet.IP.Equal(ip) {
				return iface.Index, nil
			}
		}
	}
	return -1, fmt.Errorf("no interface found for IP: %s", ip)
}

// macToIfIndex takes a MAC address and returns index of interface with matching address
func macToIfIndex(hwAddr net.HardwareAddr) (int, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return -1, fmt.Errorf("failed to get interfaces: %s", err)
	}
	for _, iface := range ifaces {
		if hwAddr.String() == iface.HardwareAddr.String() {
			return iface.Index, nil
		}
	}
	return -1, fmt.Errorf("no interface found for MAC: %s", hwAddr.String())
}

func getIfIndex(MACAddr net.HardwareAddr, ip net.IP) (int, error) {
	ipIndex := -1
	macIndex := -1
	var err error
	if ip == nil && MACAddr == nil {
		return -1, nil
	}

	if ip != nil {
		ipIndex, err = ipToIfIndex(ip)
		if err != nil {
			return -1, fmt.Errorf("failed to find interface with IP: %s", ip.String())
		}
	}

	if MACAddr != nil {
		macIndex, err = macToIfIndex(MACAddr)
		if err != nil {
			return -1, fmt.Errorf("failed to find interface with MAC: %s", MACAddr.String())
		}
	}

	switch {
	case (ipIndex >= 0 && macIndex >= 0) && (macIndex != ipIndex):
		return -1, fmt.Errorf("given MAC address and source IP do not resolve to same Interface")
	case (ipIndex >= 0 && macIndex >= 0) && (macIndex == ipIndex):
		return ipIndex, nil
	case ipIndex >= 0:
		return ipIndex, nil
	case macIndex >= 0:
		return macIndex, nil
	default:
		return -1, fmt.Errorf("no index found for given ip and/or mac")
	}
}

func (r *bsdRouter) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return r.RouteWithSrc(nil, nil, dst)
}

func (r *bsdRouter) RouteWithSrc(MACAddr net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	dstAddr := toRouteAddr(dst)
	if dstAddr == nil {
		return nil, nil, nil, fmt.Errorf("failed to parse dst: %#v", dst)
	}

	pid := os.Getpid()
	seq := rand.Int()
	msg := &route.RouteMessage{
		Version: syscall.RTM_VERSION,
		Type:    unix.RTM_GET,
		ID:      uintptr(pid),
		Seq:     seq,
		Addrs: []route.Addr{
			dstAddr,
			nil,
			nil,
			nil,
			&route.LinkAddr{},
		},
	}
	ifIndex, err := getIfIndex(MACAddr, src)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to determine ifIndex: %s", err)
	}
	if ifIndex >= 0 {
		msg.Flags = RTF_IFSCOPE
		msg.Index = ifIndex
	}

	var reply *route.RouteMessage
	if reply, err = getRouteMsgAnswer(msg); err != nil {
		return
	}
	if iface, err = net.InterfaceByIndex(reply.Index); err != nil {
		return
	}
	preferredSrc = toIPAddr(reply.Addrs[5])
	if dst.String() == preferredSrc.String() {
		return
	}
	gateway = toIPAddr(reply.Addrs[1])
	return
}

// getRouteMsgAnswer takes an RTM_GET RouteMessage and returns an answer (RouteMessage)
func getRouteMsgAnswer(m *route.RouteMessage) (rm *route.RouteMessage, err error) {
	msgTimeout := 10 * time.Second
	if m.Type != syscall.RTM_GET {
		return nil, errors.New("message type is not RTM_GET")
	}
	so, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	defer unix.Close(so)

	wb, err := m.Marshal()
	if err != nil {
		return nil, err
	}
	if _, err = unix.Write(so, wb); err != nil {
		// will return unix.ESRCH when route not found, i.e. there are no routing tables
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), msgTimeout)
	responseErr := make(chan error, 1)
	var ok bool
	var rb [2 << 10]byte
	go func() {
		defer cancel()
		for {
			if err := ctx.Err(); err != nil { // timed out
				responseErr <- ctx.Err()
				return
			}
			n, err := unix.Read(so, rb[:])
			if err != nil {
				responseErr <- fmt.Errorf("failed to read from routing socket: %s", err)
				return
			}
			// Parse the response messages
			rms, err := route.ParseRIB(route.RIBTypeRoute, rb[:n])
			if err != nil {
				responseErr <- fmt.Errorf("failed to parsed messages: %s", err)
				return
			}
			if len(rms) != 1 {
				responseErr <- fmt.Errorf("unexpected number of messages received: %d", len(rms))
				return
			}
			rm, ok = rms[0].(*route.RouteMessage)
			// confirm it is a reply to our query
			if !ok || (m.ID != rm.ID && m.Seq != rm.Seq && rm.Type != unix.RTM_GET) {
				rm = nil
				continue
			}
			responseErr <- nil
			return
		}
	}()

	err = <-responseErr
	return
}

// New return a stateless routing router
func New() (routing.Router, error) {
	rtr := &bsdRouter{}
	return rtr, nil
}
