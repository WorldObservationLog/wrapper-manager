package main

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"
)

var (
	usedPorts    = make(map[int]bool)
	portMutex    sync.Mutex
	randomSource = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func isPortAvailable(port int) bool {
	addr := net.JoinHostPort("0.0.0.0", fmt.Sprintf("%d", port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func GenerateUniquePort() int {
	const minPort = 10000
	const maxPort = 65525

	portMutex.Lock()
	defer portMutex.Unlock()

	if len(usedPorts) >= (maxPort - minPort + 1) {
		return -1
	}

	for {
		port := randomSource.Intn(maxPort-minPort+1) + minPort
		if usedPorts[port] {
			continue
		}
		if !isPortAvailable(port) {
			continue
		}
		usedPorts[port] = true
		return port
	}
}
