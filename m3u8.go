package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

func GetM3U8(ctx context.Context, instance *WrapperInstance, adamId string) (string, error) {
	// 用 context deadline 和固定超时中取较早的那个，客户端取消可立即退出。
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", instance.M3U8Port))
	if err != nil {
		instance.ReportM3U8Error()
		instance.SetReady(false)
		go KillWrapper(instance)
		return "", fmt.Errorf("dial error: %w", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(deadline); err != nil {
		instance.ReportM3U8Error()
		return "", fmt.Errorf("set deadline error: %w", err)
	}

	_, err = conn.Write([]byte{byte(len(adamId))})
	if err != nil {
		instance.ReportM3U8Error()
		if isTimeout(err) {
			instance.SetReady(false)
			go KillWrapper(instance)
		}
		return "", fmt.Errorf("conn write error: %w", err)
	}

	_, err = io.WriteString(conn, adamId)
	if err != nil {
		instance.ReportM3U8Error()
		if isTimeout(err) {
			instance.SetReady(false)
			go KillWrapper(instance)
		}
		return "", fmt.Errorf("conn write error: %w", err)
	}

	response, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		instance.ReportM3U8Error()
		if isTimeout(err) {
			instance.SetReady(false)
			go KillWrapper(instance)
		}
		return "", fmt.Errorf("conn read error: %w", err)
	}

	if len(response) > 0 {
		instance.ReportM3U8Success()
		return string(bytes.TrimSpace(response)), nil
	}
	instance.ReportM3U8Error()
	return "", errors.New("empty response")
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
