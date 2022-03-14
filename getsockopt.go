package main

import (
	"net"
	"syscall"
	"unsafe"
)

const (
	SO_ORIGINAL_DST   = 80
	SizeOfSockAddrIn4 = 16
	SizeOfSockAddrIn6 = 28
)

func errnoErr(e syscall.Errno) error {
	switch e {
	case 0:
		return nil
	case syscall.EAGAIN:
		return syscall.EAGAIN
	case syscall.EINVAL:
		return syscall.EINVAL
	case syscall.ENOENT:
		return syscall.ENOENT
	}
	return e
}

func AnyToSockAddr(rsa *syscall.RawSockaddrAny) (syscall.Sockaddr, error) {
	switch rsa.Addr.Family {
	case syscall.AF_INET:
		pp := (*syscall.RawSockaddrInet4)(unsafe.Pointer(rsa))
		sa := new(syscall.SockaddrInet4)
		p := (*[2]byte)(unsafe.Pointer(&pp.Port))
		sa.Port = int(p[0])<<8 + int(p[1])
		for i := 0; i < len(sa.Addr); i++ {
			sa.Addr[i] = pp.Addr[i]
		}
		return sa, nil

	case syscall.AF_INET6:
		pp := (*syscall.RawSockaddrInet6)(unsafe.Pointer(rsa))
		sa := new(syscall.SockaddrInet6)
		p := (*[2]byte)(unsafe.Pointer(&pp.Port))
		sa.Port = int(p[0])<<8 + int(p[1])
		sa.ZoneId = pp.Scope_id
		for i := 0; i < len(sa.Addr); i++ {
			sa.Addr[i] = pp.Addr[i]
		}
		return sa, nil
	}
	return nil, syscall.EAFNOSUPPORT
}

func SockAddrToTCP(sa syscall.Sockaddr) net.Addr {
	switch sa := sa.(type) {
	case *syscall.SockaddrInet4:
		return &net.TCPAddr{IP: sa.Addr[0:], Port: sa.Port}
	case *syscall.SockaddrInet6:
		return &net.TCPAddr{IP: sa.Addr[0:], Port: sa.Port}
	}
	return nil
}

func GetSockOpt(s int, level int, name int, val unsafe.Pointer, vallen *uint32) (err error) {
	_, _, e1 := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(s), uintptr(level), uintptr(name),
		uintptr(val), uintptr(unsafe.Pointer(vallen)), 0)
	if e1 != 0 {
		err = errnoErr(e1)
	}
	return
}

func GetSockOptInet4(fd, level, opt int) (*net.Addr, error) {
	var value syscall.RawSockaddrAny
	var sockAddr syscall.Sockaddr
	var addr net.Addr
	var err error

	valueLen := uint32(SizeOfSockAddrIn4)
	if err = GetSockOpt(fd, level, opt, unsafe.Pointer(&value), &valueLen); err != nil {
		return nil, err
	}
	if sockAddr, err = AnyToSockAddr(&value); err != nil {
		return nil, err
	}
	addr = SockAddrToTCP(sockAddr)
	return &addr, err
}

func GetSockOptInet6(fd, level, opt int) (*net.Addr, error) {
	var value syscall.RawSockaddrAny
	var sockAddr syscall.Sockaddr
	var addr net.Addr
	var err error

	valueLen := uint32(SizeOfSockAddrIn6)
	if err = GetSockOpt(fd, level, opt, unsafe.Pointer(&value), &valueLen); err != nil {
		return nil, err
	}
	if sockAddr, err = AnyToSockAddr(&value); err != nil {
		return nil, err
	}
	addr = SockAddrToTCP(sockAddr)
	return &addr, nil
}
