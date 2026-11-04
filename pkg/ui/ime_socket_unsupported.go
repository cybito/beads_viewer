//go:build !darwin && !linux

package ui

import (
	"errors"
	"net"
)

type imeSocketID struct{}

func validateIMESocket(string, bool) (imeSocketID, error) {
	return imeSocketID{}, errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func statIMESocket(string) (imeSocketID, error) {
	return imeSocketID{}, errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func imeProcessGroup() (int, error) {
	return 0, errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func checkIMESocketAlive(net.Conn) error {
	return errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}
