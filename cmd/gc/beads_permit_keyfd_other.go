//go:build !linux

package main

import (
	"crypto/sha256"
	"errors"
	"os"
)

func receiveHostBeadsPermitPrivateKeyFD(string, [sha256.Size]byte, string) (int, error) {
	return -1, errors.New("protected private-key broker is unsupported on this platform")
}

func readHostBeadsPermitPrivateKeyFD(int, int64) ([]byte, error) {
	return nil, errors.New("sealed private-key descriptors are unsupported on this platform")
}

func closeHostBeadsPermitPrivateKeyFD(fd int) error {
	file := os.NewFile(uintptr(fd), "gascity-protected-mutation-key")
	if file == nil {
		return nil
	}
	return file.Close()
}

func lockDownHostBeadsPermitProcessMemory() error {
	return errors.New("protected signing-authority process isolation is unsupported on this platform")
}
