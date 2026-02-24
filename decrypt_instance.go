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
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
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
	d.connMu.Lock()
	defer d.connMu.Unlock()
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

		d.connMu.Lock()
		if d.conn != nil {
			_ = d.conn.Close()
		}
		d.connMu.Unlock()
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
	if err := d.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	defer d.conn.SetDeadline(time.Time{})
	err := binary.Write(d.conn, binary.LittleEndian, uint32(len(sample)))
	if err != nil {
		return nil, err
	}
	_, err = d.conn.Write(sample)
	if err != nil {
		return nil, err
	}
	de := make([]byte, len(sample))
	_, err = io.ReadFull(d.conn, de)
	if err != nil {
		return nil, err
	}
	return de, nil
}

func (d *DecryptClient) switchContext(adamId string, key string) error {
	if err := d.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer d.conn.SetDeadline(time.Time{}) // time.Time{}
	if d.LastKey != "" {
		_, err := d.conn.Write([]byte{0, 0, 0, 0})
		if err != nil {
			return err
		}
	}
	if key == prefetchKey {
		_, err := d.conn.Write([]byte{byte(len(defaultId))})
		if err != nil {
			return err
		}
		_, err = io.WriteString(d.conn, defaultId)
		if err != nil {
			return err
		}
	} else {
		_, err := d.conn.Write([]byte{byte(len(adamId))})
		if err != nil {
			return err
		}
		_, err = io.WriteString(d.conn, adamId)
		if err != nil {
			return err
		}
	}
	_, err := d.conn.Write([]byte{byte(len(key))})
	if err != nil {
		return err
	}
	_, err = io.WriteString(d.conn, key)
	if err != nil {
		return err
	}
	d.stateMu.Lock()
	d.LastAdamId = adamId
	d.LastKey = key
	d.stateMu.Unlock()
	return nil
}
