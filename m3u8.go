package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

var Lock = sync.Map{}

func GetM3U8(instance *WrapperInstance, adamId string) (string, error) {
	lock, _ := Lock.LoadOrStore(instance.Id, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", instance.M3U8Port), 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial timeout or error: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(10 * time.Second)

	if err := conn.SetWriteDeadline(deadline); err != nil {
		return "", fmt.Errorf("set write deadline error: %w", err)
	}

	_, err = conn.Write([]byte{byte(len(adamId))})
	if err != nil {
		return "", fmt.Errorf("conn write 1 error: %w", err)
	}

	_, err = io.WriteString(conn, adamId)
	if err != nil {
		return "", fmt.Errorf("conn write 2 (WriteString) error: %w", err)
	}

	if err := conn.SetReadDeadline(deadline); err != nil {
		return "", fmt.Errorf("set read deadline error: %w", err)
	}

	response, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("conn read error: %w", err)
	}

	_ = conn.SetReadDeadline(time.Time{})

	if len(response) > 0 {
		response = bytes.TrimSpace(response)
		return string(response), nil
	} else {
		return "", errors.New("empty response")
	}
}
