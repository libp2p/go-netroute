package netroute

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/google/gopacket/routing"
	"golang.org/x/sys/unix"
)

const (
	PF_ROUTE = unix.AF_ROUTE

	messageTimeout        = time.Second * 15 // how long we'll wait for the system to respond
	messageExpireDuration = messageTimeout   // how long a message will remain valid in our own queue
	messageQueueLength    = 8                // arbitrary

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

// While we can have multiple connections and message loops going,
// there's not much point to this when a single router object
// can handle all clients and their requests (with a single message queue).
// So we create a pkg level shared instance that's initialized on demand
// and reaped by runtime semantics.
var (
	instanceGaurd sync.Mutex
	pkgRouter     *sharedRouter
)

// bsdRouter implements the Go netroute interface
// by utilizing the (4.3BSD-Reno, Net/2) route protocol.
// This specific implementation conforms to SunOS/Solaris/Illumos specifications.
// Considering the Net/2 lineage, it should be easily adaptable to other systems
// that utilize it. Such as the named BSDs, QNX, AIX, HP-UX, et al.
type bsdRouter struct {
	systemChannel systemChannel           // The connection between our process and the routing system
	responseQueue chan routingGetResponse // Storage for system messages that have been translated to Go format
	sequence      int32                   // Message sequence value; we provide a unique value for each message
	cancel        context.CancelFunc      // When called, all route operations should cease
}

func (r *bsdRouter) Close() error {
	if r.cancel == nil || r.systemChannel == 0 {
		return errors.New("router was never initialized")
	}
	r.cancel()
	return unix.Close(r.systemChannel) // we expect the system to complain for us if sd <= 0
}

// sharedRouter shares a single Router instance with callers.
type sharedRouter struct {
	bsdRouter
	refCount uint
}

func (r *sharedRouter) Close() error {
	instanceGaurd.Lock()
	defer instanceGaurd.Unlock()

	r.refCount--
	if r.refCount == 0 {
		pkgRouter = nil
		return r.bsdRouter.Close()
	}
	return nil
}

type (
	systemChannel      = int // currently a socket descriptor
	routingGetResponse struct {
		// Go routing interface
		iface                 *net.Interface
		gateway, preferredSrc net.IP
		// package message coordination data
		sequence   int32     // message identifier; should be unique per request/response pair in our use
		expiration time.Time // messages should be discarded if they're expired
		err        error     // will be non-nil if the router encountered an error while handling a response from the system
	}
)

func New() (routing.Router, error) {
	instanceGaurd.Lock()
	defer instanceGaurd.Unlock()

init:
	if pkgRouter != nil {
		pkgRouter.refCount++

		/* FIXME: this is still incorrect
		we want a finalizer to trigger when the reference falls out of scope
		the pkg variable is never going to fall out of scope itself
		but we also can't / don't want to return a double pointer as the interface
		this may be impossible on its own so we'll have to create a router
		and in New() dynamically construct a shared reference in some way that embeds it

		routerInstance := pkgRouter
		runtime.SetFinalizer(&routerInstance, (*sharedRouter).Close)
		return routerInstance, nil
		*/
		return pkgRouter, nil
	}

	// initialise a `route (7P)` communication channel with the system
	systemChannel, err := unix.Socket(
		PF_ROUTE,       // routing service ID
		unix.SOCK_RAW,  // direct channel to the system
		unix.AF_UNSPEC, // allow messages for any address family
	)
	if err != nil {
		return nil, err
	}

	// process messages for as long as this context is valid
	ctx, cancel := context.WithCancel(context.Background())

	pkgRouter = &sharedRouter{
		bsdRouter: bsdRouter{
			responseQueue: parseRoutingGetMessages(ctx, systemChannel),
			systemChannel: systemChannel,
			cancel:        cancel,
		},
	}

	goto init // makes more sense than duplicating instance creation code and explanation here
}

func (r *bsdRouter) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	sequence := atomic.AddInt32(&r.sequence, 1)

	defer func() { // TODO: dbg lint
		switch err {
		case nil:
			fmt.Printf("[%d]%v was routed -> %v|%v\n", sequence, dst, iface, gateway)
		case unix.ESRCH:
			fmt.Printf("[%d] Error from Route():\n\tdst: (%v)\n\t%#v\n\terr: Not in routing table\n", sequence, dst, dst)
		default:
			fmt.Printf("[%d] Error from Route():\n\tdst: (%v)\n\t%#v\n\terr: %s\n\tinterface: %#v\n\tgateway: %#v\n\tsrc: %#v\n", sequence, dst, dst, err, iface, gateway, preferredSrc)
		}
	}()

	var message []byte
	if message, err = generateRoutingGetMessage(dst, sequence); err != nil {
		return
	}

	if err = sendGetRequest(r.systemChannel, message); err != nil {
		return
	}

	return receiveGetResponse(sequence, r.responseQueue)
}

func (r *bsdRouter) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	// TODO: not sure how we target a specific interface with the protocol, if at all
	// assuming maybe tack it on after DST in either the GATEWAY or IFP slot of the addr vector?
	// we can request what gateway to use, but seemingly not which interface, needs more investigation

	// for now just duplicate route
	return r.Route(dst)
}

// TODO: cleanup error logic; need to split fatal and non-fatal
// return everything on the channel, but only stop the loop on fatal errors
func parseRoutingGetMessages(ctx context.Context, sc systemChannel) chan routingGetResponse {
	goChan := make(chan routingGetResponse, messageQueueLength)
	messageReceiveBuffer := make([]byte, messageReceiveBufferSize)
	pid := int32(unix.Getpid())

	go func() {
		var ( // static declarations; reused within loop
			header unix.RtMsghdr
			read   int
			err    error
		)

		defer func() {
			if err != nil {
				fmt.Println("closing chan: ", err) // TODO: DBG lint
				goChan <- routingGetResponse{err: err}
			}
			close(goChan)
		}()

		for {
			// ASYNC NOTE: we use blocking sockets
			// and depend on the socket provided, to be closed by the caller
			// when the provided context is canceled
			// (we shouldn't need anything more complex like IOCP, poll, etc.)
			read, err = unix.Read(sc, messageReceiveBuffer)
			switch err {
			default: // unexpected error; stop processing
				fmt.Println("fatal err:", err)
				return
			//case unix.EAGAIN, unix.EWOULDBLOCK: // no response
			case unix.ESRCH: // route was not found; non-fatal
				fmt.Println("not found:", err)
			// TODO: what are we expected to do here for the netroute API?
			// return an error? nil values?
			case nil: // no error; process message
			}

			if read < unix.SizeofRtMsghdr {
				err = fmt.Errorf("error reading routing message - bytes read are less than message header's size (%d/%d)", read, unix.SizeofRtMsghdr)
				return
			}

			reader := bytes.NewReader(messageReceiveBuffer)

			if err = binary.Read(reader, binary.LittleEndian, &header); err != nil { // TODO: native endian
				return
			}

			if header.Type != unix.RTM_GET || // we're only interested in GET responses
				header.Pid != pid { // and only the ones originating from our process
				if ctx.Err() != nil { // so drop the message and continue (unless we're canceled)
					return
				}
				continue
			}

			if read != int(header.Msglen) {
				// this should never happen
				// if it does the `messageReceiveBufferSize` const needs to be amended
				err = fmt.Errorf("our buffer was too small to fit the message (%d/%d)", read, header.Msglen)
				return
			}

			resp := routingGetResponse{
				sequence:   header.Seq,
				expiration: time.Now().Add(messageExpireDuration),
			}
			// if the system says the message was confirmed, decode it
			if header.Flags&unix.RTF_DONE != 0 {
				if resp.iface, resp.preferredSrc, resp.gateway, err = decodeGetMessage(header, reader); err != nil {
					return
				}
			} else { // otherwise just relay the error
				if header.Errno != 0 {
					resp.err = unix.Errno(header.Errno)
				}
			}

			select {
			case goChan <- resp:
			case <-ctx.Done():
				return
			}
		}
	}()

	return goChan
}

// message data is expected to conform to the specification defined in `route (7p)`
// NOTE: callers responsibility to check message length and type
// we assume the arguments are associated and of type RTM_GET
func decodeGetMessage(header unix.RtMsghdr, message io.ReadSeeker) (iface *net.Interface, src, gateway net.IP, err error) {
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
			gateway = ip // system returns loopback IP to us for this value, but we explicitly omitt it
			// is this correct behavior?
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

		// NOTE: Go doesn't let you cast ranges of bytes the way we want to
		// below is a pedantic safe way to copy the values, and a more direct way follows
		/*
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
	header := unix.RtMsghdr{
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
	message := new(bytes.Buffer)
	message.Grow(messageSize)
	if err := binary.Write(message, binary.LittleEndian, &header); err != nil { // TODO: use "native" endian, not "little"
		return nil, err
	}
	if err := binary.Write(message, binary.LittleEndian, destAddr); err != nil { // TODO: use "native" endian, not "little"
		return nil, err
	}

	return message.Bytes(), nil
}

// NOTE: [protocol - `route (7P)`]
// The system will return errors immediately from `write`/`sendmsg`
// however, it will also broadcast this message to all listeners. (that includes the source sender)
// If a non-fatal error is encountered (a defined routing error, not a general/socket error)
// the caller should expect this messages response to come back to them over the system channel
func sendGetRequest(sc systemChannel, message []byte) error {
	_, err := unix.Write(sc, message)
	return err
}

func receiveGetResponse(sequence int32, responseQueue chan routingGetResponse) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	// We account for both a total call time and provide an interrupt during receive.
	// Preventing us from looping infinitely, and blocking during the channel read forever.
	callTimeout := time.Now().Add(messageTimeout) // deadline style
	receiveTimeout := time.After(messageTimeout)  // select trigger style
	errTimeout := fmt.Errorf("system did not respond to our request before timeout: %v", callTimeout)

	// if we encounter someone else's message, we keep track of which sequence it had
	// if we encounter it again later, we'll wait in real time to prevent a tight loop.
	// (otherwise we could end up in a scenario where
	// we continuously pull the same message from the queue,
	// put it back into the queue, and repeat, very quickly)
	seen := make([]int32, 0, messageQueueLength/4)

	for {
		if callTimeout.Before(time.Now()) { // entire process took too long; abort
			err = errTimeout
			return
		}

		select {
		case <-receiveTimeout: // no response at all; abort
			err = errTimeout
			return

		case resp, ok := <-responseQueue:
			if !ok {
				return
			}

			if resp.sequence == sequence { // it's for us
				if resp.err != nil {
					err = resp.err
					return
				}

				iface, gateway, preferredSrc = resp.iface, resp.gateway, resp.preferredSrc
				return
			}

			// it's not for us
			// if it's expired, get the next message from the queue
			if resp.expiration.Before(time.Now()) {
				continue
			}

			// otherwise put it back into the queue
			responseQueue <- resp

			// if we saw this sequence before, let the thread rest for a little bit
			// giving the system a chance to respond, the message loop time to enqueue responses,
			// and other threads the chance to read their own processed messages
			seq := resp.sequence
			var sawBefore bool
			for _, sawn := range seen {
				if seq == sawn {
					runtime.Gosched()
					time.Sleep(200)
					sawBefore = true
					break
				}
			}
			if !sawBefore {
				seen = append(seen, seq)
			}
		}
	}
}
