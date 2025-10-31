package netroute

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"reflect"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/gopacket/routing"
	"golang.org/x/sys/unix"
)

// Message sequence value.
// We provide a unique value for each request,
// and utilize it to route the response to the caller.
// NOTE: This value should be handled atomically.
var sequence int32

func init() { log.SetFlags(log.Lshortfile) } // TODO: dbg lint

const (
	PF_ROUTE = unix.AF_ROUTE

	// MAGIC: note that this is not the maximum possible size of a route message
	// it is only the maximum possible size of a message that we'll be requesting.
	messageReceiveBufferSize = unix.SizeofRtMsghdr + // message header with trailing vector of
		(unix.SizeofSockaddrInet6 * // inet4 or inet6 addrs
			4) + // including vector entries DST, GATEWAY, NETMASK, GENMASK
		unix.SizeofSockaddrDatalink // along with space for the interface sockaddr IFP+IFA

		// the maximum possible message is theoretically this, but we only care about inet and inet6 addrs
		// (substitute `RTA_BRD` for whatever the last addr defined in the route vector spec currently is)
		// var messageReceiveBufferSize = unix.SizeofRtMsghdr +
		//	(unix.SizeofSockaddrAny * (bits.TrailingZeros(unix.RTA_BRD) + 1))
)

type (
	systemChannel = int // currently a socket descriptor
	// bsdRouter implements the Go netroute interface
	// by utilizing the (4.3BSD-Reno, Net/2) route protocol.
	// This specific implementation conforms to SunOS/Solaris/Illumos specifications.
	// Considering the Net/2 lineage, it should be easily adaptable to other systems
	// that utilize it. Such as the named BSDs, QNX, AIX, HP-UX, et al.
	bsdRouter struct{}
)

func New() (routing.Router, error) { return new(bsdRouter), nil }

func (r *bsdRouter) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	defer func() { // TODO: dbg lint
		switch err {
		case nil:
			log.Printf("[%d] %v was routed -> %v|%v\n", sequence, dst, iface, gateway)
		case unix.ESRCH:
			log.Printf("[%d] Error from Route():\n\tdst: (%v)\n\t%#v\n\terr: Not in routing table\n", sequence, dst, dst)
		default:
			log.Printf("[%d] Error from Route():\n\tdst: (%v)\n\t%#v\n\terr: %s\n\tinterface: %#v\n\tgateway: %#v\n\tsrc: %#v\n", sequence, dst, dst, err, iface, gateway, preferredSrc)
		}
	}()

	const messageTimeout = time.Second * 15 // how long we'll wait for the system to respond
	var (
		ourSequence = atomic.AddInt32(&sequence, 1)
		ourPid      = int32(unix.Getpid())
		router      systemChannel
		request     []byte
	)
	// initialize a `route (7P)` communication channel with the system
	router, err = unix.Socket(
		PF_ROUTE,       // routing service ID
		unix.SOCK_RAW,  // direct channel to the system
		unix.AF_UNSPEC, // allow messages for any address family
	)
	if err != nil {
		return
	}
	defer unix.Close(router)
	//log.Printf("router:%x\n", router)

	if request, err = generateRoutingGetMessage(dst, ourSequence); err != nil {
		return
	}

	err = sendGetRequest(router, request)
	switch err {
	case nil: // no error; process message
	case unix.ESRCH: // route known to not exist; don't bother reading the response (same value)
		err = fmt.Errorf("dst %v not found", dst)
		return
	default: // unexpected system error; fatal
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), messageTimeout)
	responseErr := make(chan error, 1)
	go func() {
		defer cancel()
		var (
			//header unix.RtMsghdr // TODO: bug in unix pkg, size mismatch
			header  syscall.RtMsghdr
			message = make([]byte, messageReceiveBufferSize)
		)
		for {
			err := ctx.Err()
			if err != nil { // timed out
				responseErr <- ctx.Err()
				return
			}
			// read entire message into the buffer
			read, err := unix.Read(router, message)
			switch err {
			default: // unexpected error; stop processing
				log.Printf("sock(%x) fatal err: %v\n", router, err)
				responseErr <- err
				return
			case unix.ESRCH: // route was not found; non-fatal
				log.Println("not found:", err)
				responseErr <- fmt.Errorf("dst %v not found", dst)
				return
			// TODO: what are we expected to do here for the netroute API?
			// return an error? nil values?
			case nil: // no error; process message
			}
			if read < unix.SizeofRtMsghdr {
				responseErr <- fmt.Errorf("error reading routing message - bytes read are less than message header's size (%d/%d)", read, unix.SizeofRtMsghdr)
				return
			}

			// scan message header bytes
			reader := bytes.NewReader(message)
			if err := binary.Read(reader, binary.LittleEndian, &header); err != nil { // TODO: native endian
				responseErr <- err
				return
			}
			if header.Type != unix.RTM_GET || // we're only interested in GET responses,
				header.Pid != ourPid || // originating from our process,
				header.Seq != ourSequence { // with our sequence
				continue // drop other messages
			}
			if read != int(header.Msglen) { // this should never happen
				// if it does the `messageReceiveBufferSize` const needs to be amended
				responseErr <- fmt.Errorf("our buffer was too small to fit the message (%d/%d)",
					read, header.Msglen)
				return
			}

			// scan message payload
			// if the system says the message was confirmed, decode it
			if header.Flags&unix.RTF_DONE != 0 {
				iface, preferredSrc, gateway, err = decodeGetMessage(header, reader)
			} else if header.Errno != 0 {
				err = unix.Errno(header.Errno)
			}
			responseErr <- err
			return
		}
	}()

	err = <-responseErr
	return
}

func (r *bsdRouter) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	// TODO: not sure how we target a specific interface with the protocol, if at all
	// assuming maybe tack it on after DST in either the GATEWAY or IFP slot of the addr vector?
	// we can request what gateway to use, but seemingly not which interface, needs more investigation

	// for now just duplicate route
	return r.Route(dst)
}

// message data is expected to conform to the specification defined in `route (7p)`
// NOTE: callers responsibility to check message length and type
// we assume the arguments are associated and of type RTM_GET
func decodeGetMessage(header syscall.RtMsghdr, message io.ReadSeeker) (iface *net.Interface, src, gateway net.IP, err error) {
	iface = new(net.Interface)
	iface.MTU = int(header.Rmx.Mtu)

	// NOTE: the sequence order is important
	// `route (7p)` :
	// ... sockaddrs are interpreted by position ...
	// ... the sequence is least significant to most significant bit within the vector

	// re-used interface variable
	// used to perform generic IP operations on any of the IP sockaddrs in the message vector
	var ip net.IP

	if header.Addrs&unix.RTA_DST != 0 {
		if ip, err = parseIP(message); err != nil {
			return
		}
		iface.Flags |= parseFlags(ip)
		src = ip
	}

	if header.Addrs&unix.RTA_GATEWAY != 0 {
		if ip, err = parseIP(message); err != nil {
			return
		}
		iface.Flags |= parseFlags(ip)

		if !ip.IsLoopback() { // TODO: review; test imply this should be nil when loopback?
			gateway = ip // system returns loopback IP to us for this value, but we explicitly omit it
			// is this correct behavior?
			// Why doesn't the test expect this to be populated? / Why does our system provide it?
		}
	}

	if header.Addrs&unix.RTA_NETMASK != 0 {
		if ip, err = parseIP(message); err != nil {
			return
		}
		iface.Flags |= parseFlags(ip)
	}

	if header.Addrs&unix.RTA_GENMASK != 0 {
		if ip, err = parseIP(message); err != nil {
			return
		}
		iface.Flags |= parseFlags(ip)
	}

	if header.Addrs&unix.RTA_IFP != 0 {
		var dataLink unix.RawSockaddrDatalink
		if err = binary.Read(message, binary.LittleEndian, &dataLink); err != nil {
			return
		}
		iface.Index = int(dataLink.Index)

		// NOTE: Go doesn't let you cast ranges of bytes the way we want to.
		/* This is a pedantically safe way to copy the values ...
		var (
			nameLen  = int(dataLink.Nlen)
			linkLen  = int(dataLink.Alen)
			runes    = make([]rune, nameLen)
			linkAddr = make([]byte, linkLen)
			i        int
		)
		for i = nameLen - 1; i >= 0; i-- { // extract name
			runes[i] = rune(dataLink.Data[i])
		}
		iface.Name = string(runes)

		if header.Addrs&unix.RTA_IFA != 0 && linkLen > 0 {
			for i = linkLen - 1; i >= 0; i-- { // extract addr
				linkAddr[i] = byte(dataLink.Data[nameLen+i])
			}
			iface.HardwareAddr = linkAddr
		}
		// ... but we use a more direct way below.
		*/

		nlen := dataLink.Nlen
		alen := dataLink.Alen

		nameSlice := dataLink.Data[:nlen:nlen] // copy the range of bytes
		stringDirect := (*reflect.StringHeader)(unsafe.Pointer(&iface.Name))
		stringDirect.Data = (*reflect.SliceHeader)(unsafe.Pointer(&nameSlice)).Data // circumvent type restrictions
		stringDirect.Len = int(nlen)

		if header.Addrs&unix.RTA_IFA != 0 && alen > 0 {
			macSlice := dataLink.Data[nlen : nlen+alen : nlen+alen]
			byteDirect := (*reflect.SliceHeader)(unsafe.Pointer(&iface.HardwareAddr))
			byteDirect.Data = (*reflect.SliceHeader)(unsafe.Pointer(&macSlice)).Data
			byteDirect.Len = int(alen)
			byteDirect.Cap = int(alen)
		}
	}

	if header.Flags&unix.RTF_UP != 0 {
		iface.Flags |= net.FlagUp
	}

	return
}

// TODO: review; I don't know which addresses we should be checking and when.
// Right now we just check each one returned to us and OR the flags together,
// but the flags don't seem to be matching up with system tools.
// Specifically broadcast and multicast seem to be awry.
// Need input from someone who knows networking interface standards, because I don't. -J
func parseFlags(ip net.IP) (fset net.Flags) {
	if ip.IsLoopback() {
		fset |= net.FlagLoopback
	}
	if ip.Equal(net.IPv4bcast) {
		fset |= net.FlagBroadcast
	}
	if ip.IsMulticast() {
		fset |= net.FlagMulticast
	}
	return
}

// see: sockaddr (3socket) for format specifications
// TODO: we don't need to allocate any extra sockaddrs at all
// we should instead do C style programming here via unsafe
// e.g. cast the byte slice to a sockstruct, copy the addr bytes from the offset, and return
func parseIP(reader io.ReadSeeker) (ip net.IP, err error) {
	// peek the address family of the destination contained in the message
	var tempAddr unix.RawSockaddr
	const saFamily = unsafe.Sizeof(tempAddr.Family)
	if err = binary.Read(reader, binary.LittleEndian, &tempAddr.Family); err != nil {
		return
	}
	reader.Seek(-int64(saFamily), io.SeekCurrent)

	switch tempAddr.Family {
	case unix.AF_INET:
		var netAddr unix.RawSockaddrInet4
		binary.Read(reader, binary.LittleEndian, &netAddr)
		ip = net.IP(netAddr.Addr[:])
	case unix.AF_INET6:
		var netAddr6 unix.RawSockaddrInet6
		binary.Read(reader, binary.LittleEndian, &netAddr6)
		ip = net.IP(netAddr6.Addr[:])
	default:
		err = fmt.Errorf("address family %x not expected", tempAddr.Family)
		return
	}

	return
}

func generateRoutingGetMessage(dst net.IP, sequence int32) ([]byte, error) {
	// FIXME: mismatch size in unix pkg, does not match its own constant sizeof value
	// header := unix.RtMsghdr{
	header := syscall.RtMsghdr{
		Version: unix.RTM_VERSION,
		Type:    unix.RTM_GET,
		Addrs:   unix.RTA_DST | unix.RTA_IFP,
		Seq:     sequence,
	}

	var destAddr interface{} // destination will be 1 of 2 concrete sockaddr types (v4 or v6)
	messageSize := unix.SizeofRtMsghdr

	if addr4 := dst.To4(); addr4 != nil {
		messageSize += unix.SizeofSockaddrInet4
		destSock := &unix.RawSockaddrInet4{Family: unix.AF_INET}
		copy(destSock.Addr[:], addr4[:])
		destAddr = destSock
	} else if addr6 := dst.To16(); addr6 != nil {
		messageSize += unix.SizeofSockaddrInet6
		destSock := &unix.RawSockaddrInet6{Family: unix.AF_INET6}
		copy(destSock.Addr[:], addr6[:])
		destAddr = destSock
	}

	if destAddr == nil {
		return nil, fmt.Errorf("%v - is not a IPv4 or IPv6 address", dst)
	}

	header.Msglen = uint16(messageSize)

	// serialize the header and address vector
	message := bytes.NewBuffer(make([]byte, 0, messageSize))
	if err := binary.Write(message, binary.LittleEndian, &header); err != nil { // TODO: use "native" endian, not "little"
		return nil, err
	}
	if err := binary.Write(message, binary.LittleEndian, destAddr); err != nil { // TODO: use "native" endian, not "little"
		return nil, err
	}

	return message.Bytes(), nil
}

// NOTE: [protocol - `route (7P)`]
// The system will return routing errors immediately from `write`/`sendmsg`.
// It also broadcasts this message to all listeners.
// (that includes the source sender)
// If a routing error is encountered, the caller should still expect this message's response
// to be sent to the system channel.
func sendGetRequest(sc systemChannel, message []byte) error {
	_, err := unix.Write(sc, message)
	return err
}
