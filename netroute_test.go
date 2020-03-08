package netroute

import (
	"net"
	"testing"
)

func TestRoute(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}

	// Route to 127.0.0.1 shouldn't have a gateway
	_, gw, _, err := r.Route(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if gw != nil {
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
	_, v6gw, _, err := r.Route(net.ParseIP("2607:f8b0:400a:809::200e")) // at one point google.
	if err != nil {
		t.Fatal(err)
	}
	if v6gw.Equal(gw) {
		t.Fatalf("did not expect a v4 gw for a v6 route.")
	}
}
