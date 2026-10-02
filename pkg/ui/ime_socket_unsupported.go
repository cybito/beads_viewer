//go:build !darwin && !linux

package ui

import (
	"errors"
	"net"
)

func validateIMESocket(string) error {
	return errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func checkIMESocketAlive(net.Conn) error {
	return errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}
