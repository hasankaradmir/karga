//go:build !linux

package server

import "net"

// peerUser is only known on Linux (SO_PEERCRED), where karga-server runs.
func peerUser(net.Conn) string { return "unknown" }
