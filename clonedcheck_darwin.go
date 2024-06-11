package netroute

import "syscall"

func skipCloned(flags int, routeInfo *rtInfo) bool {
	return flags&syscall.RTF_CLONING != 0 && routeInfo.Dst.String() == "0.0.0.0/0" && routeInfo.Gateway == nil
}
