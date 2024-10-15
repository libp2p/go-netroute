//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package netroute

import (
	"golang.org/x/net/route"
	"net"
	"testing"
)

func TestToIPAddr(t *testing.T) {
	for _, tc := range []struct {
		value    string
		addr     route.Addr
		mask     route.Addr
		maskCIDR int
	}{
		{
			value:    "192.168.86.1/32",
			addr:     &route.Inet4Addr{IP: [4]byte{192, 168, 86, 1}},
			mask:     &route.Inet4Addr{IP: [4]byte{255, 255, 255, 255}},
			maskCIDR: 32,
		},
		{
			value:    "192.168.86.1/24",
			addr:     &route.Inet4Addr{IP: [4]byte{192, 168, 86, 1}},
			mask:     &route.Inet4Addr{IP: [4]byte{255, 255, 255, 0}},
			maskCIDR: 24,
		},
		{
			value:    "192.168.86.1/16",
			addr:     &route.Inet4Addr{IP: [4]byte{192, 168, 86, 1}},
			mask:     &route.Inet4Addr{IP: [4]byte{255, 255, 0, 0}},
			maskCIDR: 16,
		},
		{
			value:    "192.168.86.1/8",
			addr:     &route.Inet4Addr{IP: [4]byte{192, 168, 86, 1}},
			mask:     &route.Inet4Addr{IP: [4]byte{255, 0, 0, 0}},
			maskCIDR: 8,
		},
		{
			value:    "192.168.86.1/0",
			addr:     &route.Inet4Addr{IP: [4]byte{192, 168, 86, 1}},
			mask:     &route.Inet4Addr{IP: [4]byte{0, 0, 0, 0}},
			maskCIDR: 0,
		},
		{
			value:    "::1/128",
			addr:     &route.Inet6Addr{IP: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}},
			mask:     &route.Inet6Addr{IP: [16]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255}},
			maskCIDR: 128,
		},
	} {
		t.Run(tc.value, func(t *testing.T) {
			ip, err := toIPAddr(tc.addr)
			if err != nil {
				t.Fatalf("failed parse err: %s", err)
			}
			mask, err := toIPAddr(tc.mask)
			if err != nil {
				t.Fatalf("failed parse err: %s", err)
			}
			ipn := &net.IPNet{IP: ip, Mask: net.IPMask(mask)}

			l, _ := ipn.Mask.Size()
			if l != tc.maskCIDR {
				t.Fatalf("maskCIDR %d != %d", l, tc.maskCIDR)
			}

			if ipn.String() != tc.value {
				t.Fatalf("ipn %s != %s", ipn.String(), tc.value)
			}
		})
	}
}
