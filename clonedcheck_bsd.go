//go:build dragonfly || freebsd || netbsd || openbsd

package netroute

func skipCloned(_ int, _ *rtInfo) bool {
	return false
}
