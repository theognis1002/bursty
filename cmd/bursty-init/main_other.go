//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bursty-init runs inside Linux microVMs only; build it with GOOS=linux")
	os.Exit(1)
}
