package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

const (
	defaultId   = "0"
	prefetchKey = "skd://itunes.apple.com/P000000000/s1/e1"
)

type DecryptInstance struct {
	id         string
	region     string
	conn       net.Conn
	currentKey *TaskGroupKey
	processing int32
	stopCh     chan struct{}
}

func NewDecryptInstance(inst *WrapperInstance) (*DecryptInstance, error) {
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", inst.DecryptPort))
	if err != nil {
		return nil, err
	}
	instance := &DecryptInstance{
		id:     inst.Id,
		region: inst.Region,
		conn:   conn,
		stopCh: make(chan struct{}),
	}
	return instance, nil
}

func (d *DecryptInstance) decrypt(sample []byte) ([]byte, error) {
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

func (d *DecryptInstance) switchContext(groupKey TaskGroupKey) error {
	if d.currentKey != nil {
		_, err := d.conn.Write([]byte{0, 0, 0, 0})
		if err != nil {
			return err
		}
	}
	if groupKey.Key == prefetchKey {
		_, err := d.conn.Write([]byte{byte(len(defaultId))})
		if err != nil {
			return err
		}
		_, err = io.WriteString(d.conn, defaultId)
		if err != nil {
			return err
		}
	} else {
		_, err := d.conn.Write([]byte{byte(len(groupKey.AdamId))})
		if err != nil {
			return err
		}
		_, err = io.WriteString(d.conn, groupKey.AdamId)
		if err != nil {
			return err
		}
	}
	_, err := d.conn.Write([]byte{byte(len(groupKey.Key))})
	if err != nil {
		return err
	}
	_, err = io.WriteString(d.conn, groupKey.Key)
	if err != nil {
		return err
	}
	d.currentKey = &groupKey
	return nil
}

func (d *DecryptInstance) IsProcessing() bool {
	return atomic.LoadInt32(&d.processing) != 0
}

func (s *Scheduler) AddInstance(inst *WrapperInstance) error {
	if _, exists := s.instanceMap.Load(inst.Id); exists {
		return fmt.Errorf("instance %s already exists", inst.Id)
	}
	instance, err := NewDecryptInstance(inst)
	if err != nil {
		return err
	}
	s.instanceMap.Store(inst.Id, instance)
	s.instances <- instance
	return nil
}

func (s *Scheduler) RemoveInstance(id string) error {
	inst, exists := s.instanceMap.LoadAndDelete(id)
	if !exists {
		return fmt.Errorf("instance %s not found", id)
	}
	instance := inst.(*DecryptInstance)
	close(instance.stopCh)
	if instance.conn != nil {
		_ = instance.conn.Close()
	}
	return nil
}
