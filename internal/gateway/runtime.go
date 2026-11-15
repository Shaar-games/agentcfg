package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/ipc"
	"agentcfg/internal/wire"
	"github.com/gofrs/flock"
)

const controlVersion = 1
const controlFrameLimit = 16 << 10

var errNotRunning = errors.New("gateway is not running")

type Status struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Run keeps the HTTP listener available until explicitly stopped. Individual
// upstream sessions may expire without making configured client URLs disappear.
func Run(ctx context.Context, root string, diagnostics io.Writer) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	if !cfg.Settings.Gateway.Enabled {
		return errors.New("native OAuth gateway is disabled; set gateway.enabled to true in agentcfg.json")
	}
	endpoint, err := ipc.ForService(cfg.Root, "gateway")
	if err != nil {
		return err
	}
	if err := endpoint.Prepare(); err != nil {
		return err
	}
	lifetime := flock.New(filepath.Join(endpoint.Directory, "lifetime.lock"))
	defer lifetime.Close()
	locked, err := lifetime.TryLock()
	if err != nil {
		return fmt.Errorf("lock gateway lifetime: %w", err)
	}
	if !locked {
		return errors.New("native OAuth gateway is already running")
	}
	defer lifetime.Unlock()
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	handler, err := New(runContext, cfg.Root, Options{})
	if err != nil {
		return err
	}
	defer handler.Close()
	listen := cfg.Settings.Gateway.Listen
	if listen == "" {
		listen = config.DefaultGatewayListen
	}
	listener, err := (&net.ListenConfig{}).Listen(runContext, "tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on configured local gateway address %s: %w", listen, err)
	}
	defer listener.Close()
	if cfg.Settings.Gateway.TLSCert != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.Settings.Gateway.TLSCert, cfg.Settings.Gateway.TLSKey)
		if err != nil {
			return errors.New("load gateway TLS certificate and private key")
		}
		listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	}
	control, err := ipc.Listen(endpoint)
	if err != nil {
		return err
	}
	defer control.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return runContext }}
	status := Status{Running: true, PID: os.Getpid(), URL: handler.BaseURL()}
	completed := make(chan error, 2)
	controlStopped := make(chan struct{})
	var controls sync.WaitGroup
	go func() {
		defer close(controlStopped)
		for {
			connection, err := control.Accept()
			if err != nil {
				completed <- err
				return
			}
			controls.Add(1)
			go func() {
				defer controls.Done()
				handleControl(connection, status, cancel)
			}()
		}
	}()
	go func() { completed <- server.Serve(listener) }()
	if diagnostics != nil {
		_, _ = fmt.Fprintln(diagnostics, "Native OAuth gateway listening at", handler.BaseURL())
	}
	var result error
	select {
	case <-runContext.Done():
	case err := <-completed:
		if !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	cancel()
	_ = control.Close()
	shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
	}
	_ = listener.Close()
	<-controlStopped
	controls.Wait()
	return result
}

func handleControl(connection net.Conn, status Status, stop context.CancelFunc) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := wire.NewReaderLimit(connection, controlFrameLimit)
	writer := wire.NewWriterLimit(connection, controlFrameLimit)
	request, err := reader.Read()
	if err != nil {
		return
	}
	var version int
	var operation string
	if json.Unmarshal(request["version"], &version) != nil || version != controlVersion || json.Unmarshal(request["operation"], &operation) != nil {
		_ = writer.Write(wire.Message{"error": wire.Raw("invalid gateway control request")})
		return
	}
	switch operation {
	case "status":
		_ = writer.Write(wire.Message{"ok": wire.Raw(true), "status": wire.Raw(status)})
	case "stop":
		if writer.Write(wire.Message{"ok": wire.Raw(true)}) == nil {
			stop()
		}
	default:
		_ = writer.Write(wire.Message{"error": wire.Raw("unknown gateway control operation")})
	}
}

func controlRequest(ctx context.Context, root, operation string) (wire.Message, error) {
	endpoint, err := ipc.ForService(root, "gateway")
	if err != nil {
		return nil, err
	}
	connection, err := ipc.Dial(ctx, endpoint)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", errNotRunning, err)
	}
	defer connection.Close()
	deadline := time.Now().Add(5 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, err
	}
	cancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer cancel()
	if err := wire.NewWriterLimit(connection, controlFrameLimit).Write(wire.Message{"version": wire.Raw(controlVersion), "operation": wire.Raw(operation)}); err != nil {
		return nil, err
	}
	response, err := wire.NewReaderLimit(connection, controlFrameLimit).Read()
	if err != nil {
		return nil, err
	}
	var ok bool
	if json.Unmarshal(response["ok"], &ok) != nil || !ok {
		return nil, errors.New("gateway control request was rejected")
	}
	return response, nil
}

// GetStatus never starts the gateway or creates runtime files.
func GetStatus(ctx context.Context, root string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	response, err := controlRequest(ctx, root, "status")
	if errors.Is(err, errNotRunning) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	var status Status
	if err := json.Unmarshal(response["status"], &status); err != nil || !status.Running || status.PID <= 0 || status.URL == "" {
		return Status{}, errors.New("invalid gateway status response")
	}
	return status, nil
}

func Stop(ctx context.Context, root string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := controlRequest(ctx, root, "stop")
	if err != nil {
		return err
	}
	endpoint, err := ipc.ForService(root, "gateway")
	if err != nil {
		return err
	}
	lifetime := flock.New(filepath.Join(endpoint.Directory, "lifetime.lock"))
	defer lifetime.Close()
	locked, err := lifetime.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("gateway shutdown did not complete")
	}
	return lifetime.Unlock()
}

// Start runs the service in the background using the current executable. It
// does not install login hooks or modify any MCP/harness settings.
func Start(ctx context.Context, root string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cfg, err := config.Load(root)
	if err != nil {
		return Status{}, err
	}
	if !cfg.Settings.Gateway.Enabled {
		return Status{}, errors.New("native OAuth gateway is disabled; set gateway.enabled to true in agentcfg.json")
	}
	if status, err := GetStatus(ctx, cfg.Root); err != nil {
		return Status{}, err
	} else if status.Running {
		if status.URL != config.GatewayBaseURL(cfg.Settings.Gateway) {
			return Status{}, errors.New("gateway address changed; stop the running gateway before starting it again")
		}
		return status, nil
	}
	endpoint, err := ipc.ForService(cfg.Root, "gateway")
	if err != nil {
		return Status{}, err
	}
	if err := endpoint.Prepare(); err != nil {
		return Status{}, err
	}
	startup := flock.New(filepath.Join(endpoint.Directory, "startup.lock"))
	defer startup.Close()
	locked, err := startup.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return Status{}, fmt.Errorf("lock gateway startup: %w", err)
	}
	if !locked {
		return Status{}, errors.New("gateway startup lock is unavailable")
	}
	defer startup.Unlock()
	if status, err := GetStatus(ctx, cfg.Root); err != nil {
		return Status{}, err
	} else if status.Running {
		if status.URL != config.GatewayBaseURL(cfg.Settings.Gateway) {
			return Status{}, errors.New("gateway address changed; stop the running gateway before starting it again")
		}
		return status, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return Status{}, err
	}
	logPath := filepath.Join(endpoint.Directory, "gateway.log")
	if info, err := os.Lstat(logPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return Status{}, errors.New("unsafe gateway log file")
	} else if err != nil && !os.IsNotExist(err) {
		return Status{}, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Status{}, err
	}
	defer log.Close()
	newCommand := func() *exec.Cmd {
		command := exec.Command(executable, "gateway", "run")
		command.Env = append(os.Environ(), "HOME="+cfg.Root, "USERPROFILE="+cfg.Root)
		command.Stdin = nil
		command.Stdout, command.Stderr = log, log
		ipc.Detach(command)
		return command
	}
	command := newCommand()
	if err := command.Start(); err != nil {
		retry := newCommand()
		if !ipc.RetryInInheritedJob(retry, err) {
			return Status{}, fmt.Errorf("start native OAuth gateway: %w", err)
		}
		if err := retry.Start(); err != nil {
			return Status{}, fmt.Errorf("start native OAuth gateway with inherited job: %w", err)
		}
		command = retry
	}
	defer command.Process.Release()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Status{}, fmt.Errorf("gateway did not become ready; inspect %s: %w", logPath, ctx.Err())
		case <-ticker.C:
			status, err := GetStatus(ctx, cfg.Root)
			if err == nil && status.Running {
				if status.URL != config.GatewayBaseURL(cfg.Settings.Gateway) {
					return Status{}, errors.New("a gateway with a different address is already running")
				}
				return status, nil
			}
		}
	}
}
