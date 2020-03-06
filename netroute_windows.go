// +build windows

package netroute

// Reference:
// https://docs.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-getbestroute2
import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"github.com/google/gopacket/routing"
	sockaddrnet "github.com/libp2p/go-sockaddr/net"
	"golang.org/x/sys/windows"
)

var (
	modiphlpapi = syscall.NewLazyDLL("iphlpapi.dll")

	procGetBestRoute2 = modiphlpapi.NewProc("GetBestRoute2")
)

type NetLUID uint64

type AddressPrefix struct {
	windows.Sockaddr
	PrefixLength byte
}

type RouteProtocol uint32 // MIB_IPFORWARD_PROTO

// https://docs.microsoft.com/en-us/windows/win32/api/netioapi/ns-netioapi-mib_ipforward_row2
type mib_row2 struct {
	luid              NetLUID
	index             uint32
	destinationPrefix AddressPrefix
	nextHop           windows.Sockaddr
	prefixLength      byte
	lifetime          uint32
	preferredLIfetime uint32
	metric            uint32
	protocol          RouteProtocol
	loopback          byte
	autoconfigured    byte
	publich           byte
	immortal          byte
	age               uint32
	origin            byte
}

func callBestRoute(source, dest net.IP) (*mib_row2, net.IP, error) {
	sourceAddr := sockaddrnet.IPAndZoneToSockaddr(source, "")
	destAddr := sockaddrnet.IPAndZoneToSockaddr(dest, "")
	bestRoute := make([]byte, 64)
	var bestSource windows.RawSockaddrAny

	err := getBestRoute2(nil, 0, source, dest, 0, bestRoute, bestSource)
	if err != nil {
		return nil, nil, err
	}

	// interpret best route and best source.
	route, err := parseRoute(bestRoute)
	if err != nil {
		return nil, nil, err
	}
	bestSrc, _ := sockaddrnet.SockaddrToIPAndZone(bestSource.Sockaddr())

	return &route, bestSrc, nil
}

func parseRoute(mib []byte) (*mib_row2, error) {
	var route mib_row2
	var err error

	route.luid = binary.LittleEndian.Uint64(mib[0:])
	route.index = binary.LittleEndian.Uint32(mib[8:])
	pfix, idx, err := readDestPrefix(mib, 12)
	if err != nil {
		return nil, err
	}
	route.destinationPrefix = pfix
	route.nextHop, idx, err = readSockAddr(mib, idx)
	if err != nil {
		return nil, err
	}

	return route, err
}

func readDestPrefix(buffer []byte, idx int) (*AddressPrefix, int, error) {
	sock, idx, err := readSockAddr(buffer, idx)
	if err != nil {
		return nil, 0, err
	}
	pfixLen := buffer[idx]
	return &AddressPrefix{sock, pfixLen}, idx + 1, nil
}

func readSockAddr(buffer []byte, idx int) (*windows.Sockaddr, int, error) {
	family := binary.LittleEndian.Uint16(buffer[idx:])
	if family == AF_INET {
		//14 bytes?
	} else if family == AF_INET6 {
		//24 bytes?
	} else {
		return nil, 0, fmt.Errorf("Unknown windows addr family %d", family)
	}
}

func getBestRoute2(interfaceLuid *NetLUID, interfaceIndex uint32, sourceAddress, destinationAddress []byte, addressSortOptions uint32, bestRoute []byte, bestSourceAddress []byte) (errcode error) {
	r0, _, _ := syscall.Syscall9(procGetBestRoute2.Addr(), 7,
		uintptr(unsafe.Pointer(interfaceLuid)),
		uintptr(interfaceIndex),
		uintptr(unsafe.Pointer(&sourceAddress[0])),
		uintptr(unsafe.Pointer(&destinationAddress[0])),
		uintptr(addressSortOptions),
		uintptr(unsafe.Pointer(&bestRoute[0])),
		uintptr(unsafe.Pointer(&bestSourceAddress[0])),
		0, 0)
	if r0 != 0 {
		errcode = syscall.Errno(r0)
	}
	return
}

type winRouter struct{}

func (r *winRouter) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return RouteWithSource(nil, nil, dst)
}

func (r *winRouter) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	route, pref, err := callBestRoute(src, dst)
	if err != nil {
		return nil, nil, err
	}
	return route, pref, nil
}

func New() (routing.Router, error) {
	rtr := &winRouter{}
	return rtr, nil
}
