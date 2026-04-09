package vmm

import (
	"fmt"
	"net"
	"sync"
	"time"
)

const execAgentAcquireTimeout = 20 * time.Second

type execAgentBroker struct {
	port   uint32
	closed chan struct{}
	once   sync.Once
	conns  chan net.Conn
}

func newExecAgentBroker(port uint32) *execAgentBroker {
	return &execAgentBroker{
		port:   port,
		closed: make(chan struct{}),
		conns:  make(chan net.Conn, 1),
	}
}

func (b *execAgentBroker) listen(port uint32) (net.Conn, error) {
	if port != b.port {
		return nil, fmt.Errorf("no vsock listener on port %d", port)
	}
	hostConn, guestConn := net.Pipe()
	select {
	case <-b.closed:
		_ = hostConn.Close()
		_ = guestConn.Close()
		return nil, fmt.Errorf("exec agent broker is closed")
	case b.conns <- hostConn:
		return guestConn, nil
	default:
		_ = hostConn.Close()
		_ = guestConn.Close()
		return nil, fmt.Errorf("exec agent connection backlog is full")
	}
}

func (b *execAgentBroker) acquire() (net.Conn, error) {
	timer := time.NewTimer(execAgentAcquireTimeout)
	defer timer.Stop()
	select {
	case <-b.closed:
		return nil, fmt.Errorf("exec agent broker is closed")
	case conn := <-b.conns:
		return conn, nil
	case <-timer.C:
		return nil, fmt.Errorf("exec agent connection timed out")
	}
}

func (b *execAgentBroker) close() {
	b.once.Do(func() {
		close(b.closed)
		for {
			select {
			case conn := <-b.conns:
				_ = conn.Close()
			default:
				return
			}
		}
	})
}
