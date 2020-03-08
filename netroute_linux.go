// +build linux

package netroute

import (
	"net"

	"github.com/google/gopacket/routing"
)

type router struct {
	routing.Router
}

func (r *router) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return r.RouteWithSrc(nil, nil, dst)
}

func (r *router) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	iface, gateway, preferredSrc, err = r.Router.RouteWithSrc(input, src, dst)
	if dst.Equal(preferredSrc) {
		gateway = nil
	}
}

func New() (routing.Router, error) {
	return router{routing.New()}
}
