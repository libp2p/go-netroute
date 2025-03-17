package main

import (
	"fmt"
	"net"

	netroute "github.com/abakum/go-netroute"
)

func main() {
	r, err := netroute.New()
	if err != nil {
		panic(err)
	}
	iface, gw, src, err := r.Route(net.IPv4(0, 0, 0, 0))
	fmt.Printf("%v, %v, %v, %v\n", iface, gw, src, err)
}
