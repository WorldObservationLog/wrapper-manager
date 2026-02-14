package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
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
	if d.conn != nil {
		err := d.conn.Close()
		if err != nil {
			logrus.Errorf("failed to close client conn: %s", err)
		}
	}
}

func (d *DecryptClient) GetLastAdamId() string {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.LastAdamId
}

func (d *DecryptClient) GetLastHandleTime() time.Time {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.LastHandleTime
}

func (d *DecryptClient) Process(task *Task) {
	d.connMu.Lock()
	defer d.connMu.Unlock()

	d.stateMu.Lock()
	d.LastHandleTime = time.Now()
	currentLastKey := d.LastKey
	d.stateMu.Unlock()

	if currentLastKey == "" || currentLastKey != task.Key {
		err := d.switchContext(task.AdamId, task.Key)
		if err != nil {
			// d.Unavailable() // Removed, responsibility of WrapperInstance
			task.Result <- &Result{
				Success: false,
				Data:    task.Payload,
				Error:   err,
			}
			return
		}
	}
	result, err := d.decrypt(task.Payload)
	if err != nil {
		// d.Unavailable() // Removed
		task.Result <- &Result{
			Success: false,
			Data:    task.Payload,
			Error:   err,
		}
		return
	}
	task.Result <- &Result{
		Success: true,
		Data:    result,
		Error:   nil,
	}
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
