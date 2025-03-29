package netroute

import (
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
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range addrs {
		if strings.HasPrefix(addr.Network(), "ip") {
			localAddr, _, _ = net.ParseCIDR(addr.String())
			break
		}
	}
	for _, addr := range addrs {
		if strings.HasPrefix(addr.Network(), "ip") {
			_, ipn, _ := net.ParseCIDR(addr.String())
			if ipn.IP.To4() == nil &&
				!ipn.IP.IsLoopback() &&
				!ipn.IP.IsInterfaceLocalMulticast() &&
				!ipn.IP.IsLinkLocalUnicast() &&
				!ipn.IP.IsLinkLocalMulticast() {
				hasV6 = true
				break
			}
		}
	}

	_, gw, src, err := r.Route(localAddr)
	if err != nil {
		t.Fatal(err)
	}
	// FreeBSD returns lo as IFP of route
	// root@bsd1:~/src/me/go-netroute # route -nv get 192.168.64.7
	//  route to: 192.168.64.7
	//  destination: 192.168.64.7
	//  fib: 0
	//  interface: lo0
	if gw != nil || (!src.Equal(localAddr) && !src.Equal(net.IP([]byte{127, 0, 0, 1}))) {
		t.Fatalf("Did not expect gateway for %v->%v: %v", src, localAddr, gw)
	}

	// Route to somewhere external should.
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
