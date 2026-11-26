package native

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	startTimeout = 10 * time.Second
	stopTimeout  = 2 * time.Second
)

type Options struct {
	ID         string
	Executable string
	PackageDir string
	DataDir    string
	Log        io.Writer
	Config     func() map[string]any
	Exports    []string
	WebAPI     bool
}

// Process holds one executable instance and the addresses used during its lifetime.
type Process struct {
	options   Options
	cmd       *exec.Cmd
	token     string
	socket    string
	transport *http.Transport

	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
	waitErr   error
}

// Start launches and waits for one process to report readiness.
func (s *Server) Start(ctx context.Context, opts Options) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	p := &Process{
		options: opts, token: rand.Text(), ctx: lifetime, cancel: cancel,
		ready: make(chan struct{}), done: make(chan struct{}),
	}
	socket, err := s.register(p)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prepare native plugin: %w", err)
	}
	cmd := exec.Command(opts.Executable)
	cmd.Dir = opts.PackageDir
	cmd.Env = processEnv(socket, p.token, p.socket)
	cmd.Stdout, cmd.Stderr = opts.Log, opts.Log
	configureProcess(cmd)
	p.cmd = cmd
	if err := cmd.Start(); err != nil {
		s.unregister(p)
		return nil, fmt.Errorf("execute native plugin: %w", err)
	}
	go func() {
		p.waitErr = cmd.Wait()
		_ = signalGroup(cmd, true)
		s.unregister(p)
		close(p.done)
	}()
	startup, cancelStart := context.WithTimeout(ctx, startTimeout)
	defer cancelStart()
	select {
	case <-p.ready:
		select {
		case <-p.done:
			return nil, p.ExitError()
		default:
			return p, nil
		}
	case <-p.done:
		return nil, p.ExitError()
	case <-startup.Done():
		cause := fmt.Errorf("native plugin did not report ready: %w", startup.Err())
		return nil, errors.Join(cause, p.Stop(ctx))
	}
}

// Done closes after the process exits and its credentials have been revoked.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitError reports the reason an instance went away. Call after Done.
func (p *Process) ExitError() error {
	if p.waitErr == nil {
		return errors.New("native plugin exited (status 0)")
	}
	return fmt.Errorf("native plugin exited: %w", p.waitErr)
}

// Stop revokes the token and asks the process group to exit.
func (p *Process) Stop(ctx context.Context) error {
	p.cancel()
	select {
	case <-p.done:
		return nil
	default:
	}
	termErr := signalGroup(p.cmd, false)
	timer := time.NewTimer(stopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return termErr
	case <-ctx.Done():
	case <-timer.C:
	}
	killErr := signalGroup(p.cmd, true)
	timer.Reset(stopTimeout)
	select {
	case <-p.done:
		return errors.Join(termErr, killErr)
	case <-timer.C:
		return errors.Join(termErr, killErr, errors.New("native plugin did not exit after SIGKILL"))
	}
}

func (p *Process) supports(method string) bool {
	return slices.Contains(p.options.Exports, method)
}

func processEnv(socket, token, provider string) []string {
	env := os.Environ()
	env = slices.DeleteFunc(env, func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return key == "GODESK_HOST_SOCKET" || key == "GODESK_HOST_TOKEN" || key == "GODESK_PLUGIN_SOCKET"
	})
	env = append(env, "GODESK_HOST_SOCKET="+socket, "GODESK_HOST_TOKEN="+token)
	if provider != "" {
		env = append(env, "GODESK_PLUGIN_SOCKET="+provider)
	}
	return env
}

func unixTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		IdleConnTimeout:        30 * time.Second,
		ResponseHeaderTimeout:  callTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
	}
}
