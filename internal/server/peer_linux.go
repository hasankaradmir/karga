package server

import (
	"net"
	"os/user"
	"strconv"
	"syscall"
)

// peerUser is the user of the process at the other end of a unix socket (SO_PEERCRED), as the
// kernel reports it: a client cannot claim another user.
func peerUser(c net.Conn) string {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ""
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ""
	}
	var cred *syscall.Ucred
	if err := raw.Control(func(fd uintptr) {
		cred, err = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || cred == nil {
		return ""
	}
	uid := strconv.Itoa(int(cred.Uid))
	if u, err := user.LookupId(uid); err == nil {
		return u.Username
	}
	return "uid " + uid
}
