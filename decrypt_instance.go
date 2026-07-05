package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	defaultId   = "0"
	prefetchKey = "skd://itunes.apple.com/P000000000/s1/e1"
	timeout     = 5 * time.Second
)

type DecryptClient struct {
	port           int
	conn           net.Conn
	connMu         sync.Mutex
	stateMu        sync.RWMutex
	LastAdamId     string
	LastKey        string
	LastHandleTime time.Time

	// Spooling and load balancing
	activeTasks  atomic.Int32
	targetAdamId string
	targetKey    string
	targetMu     sync.RWMutex
	isBroken     atomic.Bool
}

func NewDecryptClient(port int) (*DecryptClient, error) {
	var conn net.Conn
	var err error

	// Add retry mechanism to tolerate slight desync between Python printing "listening" and OS socket readiness
	for i := 0; i < 5; i++ {
		conn, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to dial after 5 retries: %w", err)
	}
	// 关闭 Nagle：解密为请求-应答式小包交互，避免 4 字节长度前缀等被攒包延迟。
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}
	client := &DecryptClient{
		port:           port,
		conn:           conn,
		LastAdamId:     "",
		LastKey:        "",
		LastHandleTime: time.Time{},
	}
	return client, nil
}

func (d *DecryptClient) Close() {
	d.isBroken.Store(true)
	if d.conn != nil {
		err := d.conn.Close()
		if err != nil {
			logrus.Errorf("failed to close client conn: %s", err)
		}
	}
}

// markBroken 由于网络错误引发物理断连，防止留存脏数据污染下一个请求
func (d *DecryptClient) markBroken(cause error) {
	if d.isBroken.CompareAndSwap(false, true) {
		logrus.Errorf("DecryptClient marked broken due to network error: %v", cause)

		if d.conn != nil {
			_ = d.conn.Close()
		}
	}
}

func (d *DecryptClient) GetLastAdamId() string {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.LastAdamId
}

func (d *DecryptClient) GetActiveTasks() int32 {
	return d.activeTasks.Load()
}

func (d *DecryptClient) GetTargetAdamId() string {
	d.targetMu.RLock()
	defer d.targetMu.RUnlock()
	return d.targetAdamId
}

func (d *DecryptClient) GetTargetKey() string {
	d.targetMu.RLock()
	defer d.targetMu.RUnlock()
	return d.targetKey
}

func (d *DecryptClient) SetTarget(adamId string, key string) {
	d.targetMu.Lock()
	defer d.targetMu.Unlock()
	d.targetAdamId = adamId
	d.targetKey = key
}

func (d *DecryptClient) GetLastHandleTime() time.Time {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.LastHandleTime
}

func (d *DecryptClient) IsBroken() bool {
	return d.isBroken.Load()
}

func (d *DecryptClient) Process(adamId string, key string, payload []byte) ([]byte, error) {
	d.activeTasks.Add(1)
	defer d.activeTasks.Add(-1)

	if d.IsBroken() {
		return nil, fmt.Errorf("client is broken due to previous network error")
	}

	d.connMu.Lock()
	defer d.connMu.Unlock()

	if d.IsBroken() {
		return nil, fmt.Errorf("client is broken due to previous network error")
	}

	d.stateMu.Lock()
	d.LastHandleTime = time.Now()
	currentLastKey := d.LastKey
	d.stateMu.Unlock()

	if currentLastKey == "" || currentLastKey != key {
		err := d.switchContext(adamId, key)
		if err != nil {
			d.markBroken(fmt.Errorf("switchContext failed: %w", err))
			return nil, err
		}
	}
	result, err := d.decrypt(payload)
	if err != nil {
		d.markBroken(fmt.Errorf("decrypt failed: %w", err))
		return nil, err
	}
	return result, nil
}

func (d *DecryptClient) decrypt(sample []byte) ([]byte, error) {
	if len(sample) == 0 {
		return []byte{}, nil
	}
	if err := d.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	defer d.conn.SetDeadline(time.Time{})
	// 合并 4 字节长度前缀与 payload 为单次 Write，减少高频小 sample 场景下的 syscall 数。
	buf := make([]byte, 4+len(sample))
	binary.LittleEndian.PutUint32(buf[:4], uint32(len(sample)))
	copy(buf[4:], sample)
	if _, err := d.conn.Write(buf); err != nil {
		return nil, err
	}
	de := make([]byte, len(sample))
	_, err := io.ReadFull(d.conn, de)
	if err != nil {
		return nil, err
	}
	return de, nil
}

func (d *DecryptClient) switchContext(adamId string, key string) error {
	if err := d.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer d.conn.SetDeadline(time.Time{})

	// 将 reset 标记、adamId、key 的多段小写入合并到一个缓冲区一次性发出，减少 syscall。
	var buf []byte
	if d.LastKey != "" {
		buf = append(buf, 0, 0, 0, 0)
	}
	id := adamId
	if key == prefetchKey {
		id = defaultId
	}
	buf = append(buf, byte(len(id)))
	buf = append(buf, id...)
	buf = append(buf, byte(len(key)))
	buf = append(buf, key...)

	if _, err := d.conn.Write(buf); err != nil {
		return err
	}

	d.stateMu.Lock()
	d.LastAdamId = adamId
	d.LastKey = key
	d.stateMu.Unlock()
	return nil
}
