// +build linux

package netroute

import (
	"net"

	"github.com/google/gopacket/routing"
)

func New() (routing.Router, error) {
	return routing.New()
}
