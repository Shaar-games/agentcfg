package broker

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"agentcfg/internal/wire"
)

var nextPeer atomic.Uint64

type peer struct {
	id          uint64
	connection  net.Conn
	out         chan wire.Message
	done        chan struct{}
	closeOnce   sync.Once
	initialized atomic.Int32
	authorizes  bool
	authMu      sync.Mutex
	authPending map[string]chan authorizationResponse
}

func newPeer(connection net.Conn) *peer {
	p := &peer{id: nextPeer.Add(1), connection: connection, out: make(chan wire.Message, 8), done: make(chan struct{})}
	return p
}

func (p *peer) send(message wire.Message) {
	if p.authorizes {
		var err error
		message, err = encodeMCPEnvelope(message)
		if err != nil {
			p.close()
			return
		}
	}
	p.sendFrame(message)
}

func (p *peer) sendFrame(message wire.Message) {
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
	writer := wire.NewWriterLimit(p.connection, maxIPCFrameSize)
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
