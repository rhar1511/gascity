//go:build !linux

// gc-protected-authority-launcher requires Linux pidfds and Unix credentials.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gc-protected-authority-launcher requires Linux pidfds") //nolint:errcheck
	os.Exit(1)
}
