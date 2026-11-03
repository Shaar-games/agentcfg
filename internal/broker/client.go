package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/ipc"
	"agentcfg/internal/wire"

	"github.com/gofrs/flock"
)

func hello(operation string) wire.Message {
	return wire.Message{"version": wire.Raw(IPCVersion), "operation": wire.Raw(operation)}
}

func open(ctx context.Context, root string) (net.Conn, error) {
	endpoint, err := ipc.ForRoot(root)
	if err != nil {
		return nil, err
	}
	return ipc.Dial(ctx, endpoint)
}

func exchange(connection net.Conn, request wire.Message) (*wire.Reader, wire.Message, error) {
	_ = connection.SetDeadline(time.Now().Add(15 * time.Second))
	if err := wire.NewWriter(connection).Write(request); err != nil {
		return nil, nil, err
	}
	reader := wire.NewReader(connection)
	response, err := reader.Read()
	_ = connection.SetDeadline(time.Time{})
	if err != nil {
		return nil, nil, err
	}
	if raw := response["error"]; raw != nil {
		var message string
		_ = json.Unmarshal(raw, &message)
		return nil, nil, errors.New(message)
	}
	var ok bool
	_ = json.Unmarshal(response["ok"], &ok)
	if !ok {
		return nil, nil, errors.New("invalid agentcfg IPC acknowledgement")
	}
	return reader, response, nil
}

// GetStatus never starts a broker. A missing endpoint is reported as stopped.
// An idle broker can close its IPC listener between Dial and the status reply,
// especially on Windows named pipes. Only a transport-disconnect error is
// retried: malformed responses and explicit broker errors must remain visible.
func GetStatus(ctx context.Context, root string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		connection, err := open(ctx, root)
		if err != nil {
			return Status{Backends: []BackendStatus{}}, nil
		}
		_, response, err := exchange(connection, hello("status"))
		_ = connection.Close()
		if err != nil {
			if attempt < 2 && ctx.Err() == nil && statusConnectionClosed(err) {
				// The broker may have just stopped: retrying Dial will report a
				// missing endpoint as stopped, or reach a replacement broker.
				continue
			}
			return Status{}, err
		}
		var status Status
		err = json.Unmarshal(response["status"], &status)
		return status, err
	}
	return Status{}, errors.New("status probe retry limit reached")
}

// Stop requests shutdown of this source root only; unrelated brokers survive.
func Stop(ctx context.Context, root string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := open(ctx, root)
	if err != nil {
		return fmt.Errorf("broker is not reachable: %w", err)
	}
	defer connection.Close()
	_, _, err = exchange(connection, hello("stop"))
	return err
}

func ensure(ctx context.Context, root string) (net.Conn, error) {
	endpoint, err := ipc.ForRoot(root)
	if err != nil {
		return nil, err
	}
	if connection, err := ipc.Dial(ctx, endpoint); err == nil {
		return connection, nil
	}
	if err := endpoint.Prepare(); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(endpoint.Directory, "startup.lock"))
	defer lock.Close()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil || !locked {
		return nil, fmt.Errorf("could not acquire broker startup lock: %w", err)
	}
	if connection, err := ipc.Dial(ctx, endpoint); err == nil {
		return connection, nil
	}
	if err := spawn(root, endpoint); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("broker did not become ready; inspect %s: %w", filepath.Join(endpoint.Directory, "broker.log"), ctx.Err())
		case <-ticker.C:
			if connection, err := ipc.Dial(ctx, endpoint); err == nil {
				return connection, nil
			}
		}
	}
}

func spawn(root string, endpoint ipc.Endpoint) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(endpoint.Directory, "broker.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() > 4<<20 {
		_ = os.Remove(logPath + ".previous")
		_ = os.Rename(logPath, logPath+".previous")
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	newCommand := func() *exec.Cmd {
		cmd := exec.Command(executable, "daemon")
		cmd.Stdin = nil
		cmd.Stdout, cmd.Stderr = log, log
		ipc.Detach(cmd)
		return cmd
	}
	command := newCommand()
	if err := command.Start(); err != nil {
		// A failed Start marks exec.Cmd as used. Retrying that same Cmd always
		// fails with "exec: already started", even if its SysProcAttr changes.
		retry := newCommand()
		if !ipc.RetryInInheritedJob(retry, err) {
			return fmt.Errorf("start detached broker: %w", err)
		}
		if retryErr := retry.Start(); retryErr != nil {
			return fmt.Errorf("start detached broker with inherited job fallback: %w (breakaway failure: %v)", retryErr, err)
		}
		command = retry
		_, _ = fmt.Fprintln(log, "warning: Windows Job Object denied breakaway; the broker may stop when the outer job closes")
	}
	return command.Process.Release()
}

// Connect is a transparent stdio proxy after a private IPC handshake. EOF on
// either side tears down this client, not the shared upstream process.
func Connect(ctx context.Context, root, name, harness string, input io.Reader, output io.Writer) error {
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	entry, ok := cfg.Servers[name]
	if !ok || !config.Allows(entry.Harnesses, harness) || !harnessEnabled(cfg.Settings.Harnesses, harness) {
		return fmt.Errorf("unknown MCP server or excluded harness: %s", name)
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	connection, err := ensure(startup, cfg.Root)
	cancel()
	if err != nil {
		return err
	}
	defer connection.Close()
	request := hello("connect")
	request["server"], request["harness"] = wire.Raw(name), wire.Raw(harness)
	reader, _, err := exchange(connection, request)
	if err != nil {
		return err
	}
	finished := make(chan error, 2)
	go func() {
		_, err := io.Copy(connection, input)
		finished <- err
	}()
	go func() {
		writer := wire.NewWriter(output)
		for {
			message, err := reader.Read()
			if err != nil {
				finished <- err
				return
			}
			if err := writer.Write(message); err != nil {
				finished <- err
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-finished:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}
