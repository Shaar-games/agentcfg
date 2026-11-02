package broker

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"agentmux/internal/wire"
)

var nextPeer atomic.Uint64

type peer struct {
	id          uint64
	connection  net.Conn
	out         chan wire.Message
	done        chan struct{}
	closeOnce   sync.Once
	initialized atomic.Int32
}

func newPeer(connection net.Conn) *peer {
	p := &peer{id: nextPeer.Add(1), connection: connection, out: make(chan wire.Message, 8), done: make(chan struct{})}
	return p
}

func (p *peer) send(message wire.Message) {
	select {
	case <-p.done:
		return
	default:
	}
	select {
	case p.out <- message:
	case <-p.done:
	default:
		// A slow consumer must never block all clients of a shared backend.
		p.close()
	}
}

func (p *peer) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		_ = p.connection.Close()
	})
}

func (p *peer) writeLoop() {
	writer := wire.NewWriter(p.connection)
	for {
		select {
		case <-p.done:
			return
		case message := <-p.out:
			_ = p.connection.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := writer.Write(message); err != nil {
				p.close()
				return
			}
		}
	}
}
