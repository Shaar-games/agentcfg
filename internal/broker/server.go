// Package broker multiplexes independent harness stdio connections onto one
// upstream MCP process per resolved server configuration and source root.
package broker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/ipc"
	"agentcfg/internal/upstream"
	"agentcfg/internal/wire"

	"github.com/gofrs/flock"
)

const IPCVersion = 2

// Keep status/stop usable against an older broker so an upgrade can restart it.
const controlIPCVersion = 1

type BackendStatus struct {
	Name     string `json:"name"`
	PID      int    `json:"pid"`
	Clients  int    `json:"clients"`
	Pending  int    `json:"pending"`
	Isolated bool   `json:"isolated"`
}

type Status struct {
	Running  bool            `json:"running"`
	PID      int             `json:"pid,omitempty"`
	Root     string          `json:"root,omitempty"`
	Backends []BackendStatus `json:"backends"`
}

type server struct {
	ctx      context.Context
	cancel   context.CancelFunc
	root     string
	idle     time.Duration
	timeout  time.Duration
	stderr   io.Writer
	mu       sync.Mutex
	backends map[string]*backend
	lastUsed time.Time
	clients  int
}

func duration(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// Serve owns the endpoint until cancellation or the configured idle timeout.
// A cross-process lifetime lock makes startup races harmless.
func Serve(ctx context.Context, root string, stderr io.Writer) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	endpoint, err := ipc.ForRoot(cfg.Root)
	if err != nil {
		return err
	}
	if err := endpoint.Prepare(); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(endpoint.Directory, "broker.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("an agentcfg broker already owns this source root")
	}
	defer lock.Close()
	listener, err := ipc.Listen(endpoint)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{ctx: ctx, cancel: cancel, root: cfg.Root, stderr: stderr,
		idle:     duration(cfg.Settings.Runtime.IdleTimeout, 5*time.Minute),
		timeout:  duration(cfg.Settings.Runtime.RequestTimeout, 5*time.Minute),
		backends: make(map[string]*backend), lastUsed: time.Now()}
	defer s.close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		s.close()
	}()
	go s.sweep()
	var handlers sync.WaitGroup
	defer func() {
		cancel()
		_ = listener.Close()
		s.close()
		handlers.Wait()
	}()
	slots := make(chan struct{}, 128)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { <-slots }()
				s.handleConnection(connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (s *server) handleConnection(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader, writer := wire.NewReader(connection), wire.NewWriter(connection)
	hello, err := reader.Read()
	if err != nil {
		return
	}
	var version int
	_ = json.Unmarshal(hello["version"], &version)
	var operation string
	_ = json.Unmarshal(hello["operation"], &operation)
	legacyControl := version == controlIPCVersion && (operation == "status" || operation == "stop")
	if version != IPCVersion && !legacyControl {
		_ = writer.Write(wire.Message{"error": wire.Raw("unsupported agentcfg IPC version; restart the broker after upgrading")})
		return
	}
	switch operation {
	case "status":
		_ = writer.Write(wire.Message{"ok": wire.Raw(true), "status": wire.Raw(s.status())})
	case "stop":
		_ = writer.Write(wire.Message{"ok": wire.Raw(true)})
		s.cancel()
	case "connect":
		_ = connection.SetReadDeadline(time.Time{})
		s.connect(connection, reader, writer, hello)
	default:
		_ = writer.Write(wire.Message{"error": wire.Raw("unknown IPC operation")})
	}
}

func (s *server) connect(connection net.Conn, reader *wire.Reader, writer *wire.Writer, hello wire.Message) {
	var name, harness string
	_ = json.Unmarshal(hello["server"], &name)
	_ = json.Unmarshal(hello["harness"], &harness)
	cfg, err := config.Load(s.root)
	if err != nil {
		_ = writer.Write(wire.Message{"error": wire.Raw(err.Error())})
		return
	}
	entry, ok := cfg.Servers[name]
	if !ok || !config.Allows(entry.Harnesses, harness) || !harnessEnabled(cfg.Settings.Harnesses, harness) {
		_ = writer.Write(wire.Message{"error": wire.Raw("unknown MCP server or harness excluded by canonical configuration: " + name)})
		return
	}
	entry.Name = name
	entry, err = withClientWorkingDirectory(entry, hello["cwd"])
	if err != nil {
		_ = writer.Write(wire.Message{"error": wire.Raw(err.Error())})
		return
	}
	p := newPeer(connection)
	defer p.close()
	b, err := s.acquire(entry, p)
	if err != nil {
		_ = writer.Write(wire.Message{"error": wire.Raw(err.Error())})
		return
	}
	defer s.release(b, p)
	if err := writer.Write(wire.Message{"ok": wire.Raw(true)}); err != nil {
		return
	}
	go p.writeLoop()
	for {
		message, err := reader.Read()
		if err != nil {
			return
		}
		b.handle(p, message)
	}
}

func harnessEnabled(selected []string, harness string) bool {
	if harness == "" {
		return true // Explicit direct CLI connection, not a registered harness.
	}
	for _, name := range selected {
		if name == harness {
			return true
		}
	}
	return false
}

func (s *server) acquire(entry config.Server, p *peer) (*backend, error) {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s:%x", entry.Name, sha256.Sum256(encoded))
	if entry.Sharing == "isolated" {
		key += fmt.Sprintf(":%d", p.id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, fmt.Errorf("broker is shutting down; reconnect")
	}
	b := s.backends[key]
	if b == nil || b.ctx.Err() != nil {
		transport, err := upstream.Start(s.ctx, entry, s.stderr)
		if err != nil {
			return nil, fmt.Errorf("start %s: %w", entry.Name, err)
		}
		b = newBackend(s.ctx, entry, transport, s.timeout)
		s.backends[key] = b
	}
	if err := b.attach(p); err != nil {
		return nil, err
	}
	s.clients++
	s.lastUsed = time.Now()
	return b, nil
}

func (s *server) release(b *backend, p *peer) {
	b.detach(p)
	s.mu.Lock()
	s.clients--
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *server) status() Status {
	status := Status{Running: true, PID: os.Getpid(), Root: s.root, Backends: []BackendStatus{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.backends {
		if b.ctx.Err() != nil {
			continue
		}
		b.mu.Lock()
		status.Backends = append(status.Backends, BackendStatus{Name: b.name, PID: b.transport.PID(),
			Clients: len(b.peers), Pending: len(b.pending), Isolated: b.isolated})
		b.mu.Unlock()
	}
	sort.Slice(status.Backends, func(i, j int) bool { return status.Backends[i].Name < status.Backends[j].Name })
	return status
}

func (s *server) close() {
	s.mu.Lock()
	backends := make([]*backend, 0, len(s.backends))
	for _, b := range s.backends {
		backends = append(backends, b)
	}
	s.mu.Unlock()
	for _, b := range backends {
		b.close()
	}
}

func (s *server) sweep() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.reap(now)
		}
	}
}

func (s *server) reap(now time.Time) {
	s.mu.Lock()
	var idle []*backend
	for key, b := range s.backends {
		b.mu.Lock()
		unused := len(b.peers) == 0 && now.Sub(b.lastUsed) >= s.idle
		b.mu.Unlock()
		if unused || b.ctx.Err() != nil {
			delete(s.backends, key)
			idle = append(idle, b)
		}
	}
	shouldStop := s.clients == 0 && now.Sub(s.lastUsed) >= s.idle
	if shouldStop {
		s.cancel()
	}
	s.mu.Unlock()
	for _, b := range idle {
		b.close()
	}
}
