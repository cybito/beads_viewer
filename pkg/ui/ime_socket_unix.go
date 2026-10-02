//go:build darwin || linux

package ui

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// The private immediate parent protects the socket name. All preceding path
// components must be trustworthy as well; production never follows symlinks.
func validateIMESocket(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("IME socket must be a clean absolute Unix path")
	}
	uid := uint32(os.Getuid())
	parent := filepath.Dir(path)
	for component := path; ; component = filepath.Dir(component) {
		info, err := os.Lstat(component)
		if err != nil {
			return fmt.Errorf("inspect IME socket path: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("IME socket path has an unverified owner or symlink")
		}
		if component == path {
			if info.Mode()&os.ModeSocket == 0 || stat.Uid != uid || info.Mode().Perm()&0077 != 0 {
				return errors.New("IME socket must be private and owned by the current UID")
			}
		} else {
			if !info.IsDir() || (stat.Uid != uid && stat.Uid != 0) {
				return errors.New("IME socket ancestor has an untrusted owner or type")
			}
			if component == parent && (stat.Uid != uid || info.Mode().Perm()&0077 != 0) {
				return errors.New("IME socket directory must be private and owned by the current UID")
			}
			if info.Mode().Perm()&0022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0 && component != parent) {
				return errors.New("IME socket ancestor is writable by another user")
			}
		}
		if component == filepath.Dir(component) {
			break
		}
	}
	return nil
}

// A coalesced Herdr key must still observe EOF. Nonblocking peek never consumes
// a frame and treats unsolicited bytes as protocol failure, not a spare ACK.
func checkIMESocketAlive(conn net.Conn) error {
	unixConn, ok := conn.(syscall.Conn)
	if !ok {
		return errors.New("IME transport is not a local Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		var peek [1]byte
		n, _, err := syscall.Recvfrom(int(fd), peek[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		switch {
		case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EWOULDBLOCK):
		case err != nil:
			socketErr = err
		case n == 0:
			socketErr = io.EOF
		default:
			socketErr = errors.New("unsolicited data on IME intent stream")
		}
	})
	if err != nil {
		return err
	}
	return socketErr
}
