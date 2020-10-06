package main

import (
	"fmt"
	"log"
	"net"
	"runtime"
	"time"

	"github.com/libp2p/go-netroute"
)

func main() {
	router, err := netroute.New()
	if err != nil {
		log.Fatal(err)
	}

	{
		router2, err := netroute.New()
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("R1:(%T)%p\nR2:(%T)%p\n", router, router, router2, router2)
		fmt.Printf("R1:%#v\nR2:%#v\n", router, router2)
	}

	/*
		semaphore := make(chan struct{})
		go func() {
			const PF_ROUTE = unix.AF_ROUTE
			// initialise `route` (7P) communication channel with the kernel
			socketDescriptor, err := unix.Socket(
				PF_ROUTE,
				unix.SOCK_RAW,
				unix.AF_UNSPEC,
			)

			if err != nil {
				log.Fatal(err)
			}
			for {
				fmt.Println("loop")
				b := make([]byte, 4096) // arbitrary
				nr, err := unix.Read(socketDescriptor, b)
				if err != nil {
					log.Printf("Read failed: %s", err)
					continue
				}
				//fmt.Println("Recv: ", b[:nr])
				_ = nr
				var header unix.RtMsghdr
				reader := bytes.NewReader(b)
				if err = binary.Read(reader, binary.LittleEndian, &header); err != nil {
					return
				}
				fmt.Printf("%#v\n", header)
				fmt.Println("header pid:", header.Pid)
				fmt.Println("our pid:", unix.Getpid())
				fmt.Println("loop received seq:", header.Seq)
			}
		}()
	*/

	for _, dst := range []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv4(0, 0, 0, 0),
		net.IPv4(192, 168, 1, 1),
		net.IPv4(8, 8, 8, 8),
		net.ParseIP("::1"),
		net.ParseIP("::"),
		net.ParseIP("2607:f8b0:400a:809::200e"), // NOTE: this fails with ESRCH on machines that have no v6 routes (expected)
	} {
		fmt.Println("dst:", dst)
		intf, gateway, prefSrc, err := router.Route(dst)
		if err != nil {
			log.Print(err)
		}
		if intf != nil {
			fmt.Printf("res:\n\tintf: %#v\n\tgateway: %#v\n\tsrc: %#v\n", *intf, gateway, prefSrc)
			fmt.Printf("flags: %v\n\n", intf.Flags)
		} else {
			fmt.Printf("res:\n\tintf: %#v\n\tgateway: %#v\n\tsrc: %#v\n\n", intf, gateway, prefSrc)
		}
	}

	/*
			socketDescriptor, err := unix.Socket(
				unix.AF_ROUTE,
				unix.SOCK_RAW,
				unix.AF_UNSPEC,
			)

			header := unix.RtMsghdr{
				Version: unix.RTM_VERSION,
				Type:    unix.RTM_GET,
				//Pid:     int32(unix.Getpid()),
				Pid:    666,
				Addrs:  unix.RTA_DST | unix.RTA_IFP,
				Seq:    999,
				Msglen: unix.SizeofRtMsghdr + unix.SizeofSockaddrInet4,
			}

			message := new(bytes.Buffer)
			message.Grow(int(header.Msglen))
			destAddr := unix.RawSockaddrInet4{
				Family: unix.AF_INET,
				Addr:   [4]byte{127, 0, 0, 1},
			}

			if err := binary.Write(message, binary.LittleEndian, &header); err != nil { // TODO: use "native" endian, not "little"
				log.Fatal(err)
			}
			if err := binary.Write(message, binary.LittleEndian, destAddr); err != nil { // TODO: use "native" endian, not "little"
				log.Fatal(err)
			}

			_, err = unix.Write(socketDescriptor, message.Bytes())
			if err != nil {
				fmt.Printf("Error: %#v\n", err) // TODO: dbg lint
				// TODO: we should probably close here; or retry if we can
				return
			}

		<-semaphore
	*/

	runtime.GC()
	runtime.GC()
	runtime.Gosched()
	runtime.GC()
	time.Sleep(5 * time.Second)

	fmt.Println("router is about to be out of scope")
	fmt.Println(router)

	runtime.GC()
	time.Sleep(5 * time.Second)
}
