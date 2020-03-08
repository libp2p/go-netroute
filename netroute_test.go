package netroute

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestRoute(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ifs, err := net.Interfaces()
	if err != nil || len(ifs) == 0 {
		t.Skip("Can't test routing without access to system interfaces")
	}

	var localAddr net.IP
	var hasV6 bool
	addrs, err := ifs[0].Addrs()
	for _, addr := range addrs {
		if strings.HasPrefix(addr.Network(), "ip") {
			localAddr, _, _ = net.ParseCIDR(addr.String())
			break
		}
	}
	for _, addr := range addrs {
		if strings.HasPrefix(addr.Network(), "ip") {
			_, ipn, _ := net.ParseCIDR(addr.String())
			if ipn.IP.To4() == nil && !ipn.IP.IsInterfaceLocalMulticast() && !ipn.IP.IsLinkLocalUnicast() && !ipn.IP.IsLinkLocalMulticast() {
				fmt.Printf("See IPv6 Interface: %v\n", addr)
				hasV6 = true
				break
			}
		}
	}

	iface, gw, src, err := r.Route(localAddr)
	if err != nil {
		t.Fatal(err)
	}
	if gw != nil || !src.Equal(localAddr) {
		fmt.Printf("iface for localhost is %v\n", iface)
		fmt.Printf("when routing to %v, saw %v\n", localAddr, src)
		t.Fatalf("Did not expect gateway to localhost: %v", gw)
	}

	// Route to somewher external should.
	_, gw, _, err = r.Route(net.IPv4(8, 8, 8, 8))
	if err != nil {
		t.Fatal(err)
	}
	if gw == nil {
		t.Fatalf("Did not expect direct link to 8.8.8.8. Are you Google?")
	}

	// Route to v4 and v6 should differ.
	if !hasV6 {
		return
	}
	_, v6gw, _, err := r.Route(net.ParseIP("2607:f8b0:400a:809::200e")) // at one point google.
	if err != nil {
		t.Fatal(err)
	}
	if v6gw.Equal(gw) {
		t.Fatalf("did not expect a v4 gw for a v6 route.")
	}
}
