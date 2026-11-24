// Package lua runs plugin Lua code on a dedicated goroutine per plugin instance.
package lua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/EziosWJ/Extension-Host/hostsys/services"
	"github.com/EziosWJ/Extension-Host/internal/schedule"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

// ErrStopped is returned when the runtime is no longer running.
var ErrStopped = errors.New("plugin instance is stopped")

// ErrExited is returned when the plugin calls os.exit during startup.
var ErrExited = errors.New("plugin exited during startup")

// Request is the request passed to a Lua HTTP handler.
type Request struct {
	Method     string
	Path       string
	Query      map[string]string
	Headers    map[string]string
	Body       []byte
	RemoteAddr string
}

// Response is the result of a Lua HTTP handler.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// ServicesOptions is the service access granted to one plugin. A nil value or
// a missing unit entry means the plugin has no access to that operation.
type ServicesOptions struct {
	Backend services.PluginBackend
	Units   map[string]ServicePermissions
}

// ServicePermissions lists the service operations granted for one unit.
type ServicePermissions struct {
	List            bool
	Get             bool
	Restart         bool
	RestartElevated bool
}

// Options configures a plugin Lua runtime.
type Options struct {
	EntryPath   string
	PackageRoot string
	DataDir     string
	Logger      *slog.Logger
	Config      func() map[string]any
	Storage     *store.Business
	Emit        func(event string, data any) error
	Services    *ServicesOptions
}

// Runtime owns one GopherLua state.
type Runtime struct {
	opts     Options
	entryDir string
	ctx      context.Context
	cancel   context.CancelFunc
	cmds     chan command
	ready    chan error
	done     chan struct{}

	processExits chan processExit
	processes    sync.WaitGroup

	scheduler     *schedule.Scheduler
	schedules     map[string]*scheduleEntry
	scheduleFires chan scheduleFire

	startMu sync.Mutex
	started bool
	stop    sync.Once

	routes       map[string]*glua.LFunction
	routeMethods map[string]map[string]bool
	hooks        map[string]*glua.LFunction
	modules      map[string]glua.LValue
	loading      map[string]bool

	exitRequested   bool
	cancelExecution context.CancelFunc
}

type commandKind int

const (
	commandHandle commandKind = iota
	commandStop
)

type command struct {
	kind commandKind
	req  Request
	done chan result
}

type result struct {
	response Response
	err      error
}

// New prepares a runtime. Start must be called before Handle.
func New(opts Options) *Runtime {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{
		opts:          opts,
		entryDir:      filepath.Dir(opts.EntryPath),
		ctx:           ctx,
		cancel:        cancel,
		cmds:          make(chan command),
		ready:         make(chan error, 1),
		done:          make(chan struct{}),
		processExits:  make(chan processExit),
		schedules:     make(map[string]*scheduleEntry),
		scheduleFires: make(chan scheduleFire),
		routes:        make(map[string]*glua.LFunction),
		routeMethods:  make(map[string]map[string]bool),
		hooks:         make(map[string]*glua.LFunction),
		modules:       make(map[string]glua.LValue),
		loading:       make(map[string]bool),
	}
}

// Start loads the entry script and runs its start hook.
func (r *Runtime) Start(ctx context.Context) error {
	r.startMu.Lock()
	if r.started {
		r.startMu.Unlock()
		return errors.New("lua runtime already started")
	}
	r.started = true
	r.startMu.Unlock()

	go r.loop()
	select {
	case err := <-r.ready:
		if err != nil {
			r.cancel()
			<-r.done
			return err
		}
		return nil
	case <-ctx.Done():
		r.cancel()
		<-r.done
		return ctx.Err()
	}
}

// Handle dispatches one HTTP request to a registered Lua handler.
func (r *Runtime) Handle(ctx context.Context, req Request) (Response, error) {
	reply := make(chan result, 1)
	select {
	case r.cmds <- command{kind: commandHandle, req: req, done: reply}:
	case <-r.ctx.Done():
		return Response{}, ErrStopped
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	select {
	case res := <-reply:
		return res.response, res.err
	case <-r.ctx.Done():
		return Response{}, ErrStopped
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Stop runs the stop hook, closes the Lua state and releases the goroutine.
func (r *Runtime) Stop(ctx context.Context) error {
	var stopErr error
	r.stop.Do(func() {
		reply := make(chan result, 1)
		sent := false
		select {
		case r.cmds <- command{kind: commandStop, done: reply}:
			sent = true
		case <-r.ctx.Done():
		case <-ctx.Done():
			stopErr = ctx.Err()
		}
		if sent {
			select {
			case res := <-reply:
				stopErr = res.err
			case <-r.ctx.Done():
			case <-ctx.Done():
				stopErr = ctx.Err()
			}
		}
		r.cancel()
		// Give the Lua goroutine a short grace period to unwind even if the
		// caller's context is already done, so storage is not closed under it.
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
	})
	return stopErr
}

// Done is closed after the Lua state has been released.
func (r *Runtime) Done() <-chan struct{} { return r.done }

func (r *Runtime) loop() {
	defer close(r.done)
	defer func() {
		r.cancel()
		r.clearSchedules()
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r.scheduler.Stop(stopCtx)
		r.processes.Wait()
	}()
	L := glua.NewState(glua.Options{SkipOpenLibs: true})
	defer L.Close()
	execution, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.cancelExecution = cancel
	L.SetContext(execution)

	r.scheduler = schedule.New(r.opts.Logger)

	if err := r.setup(L); err != nil {
		r.ready <- err
		return
	}
	err := r.loadEntry(L)
	if err == nil {
		if hookErr := r.callHook(L, "start"); hookErr != nil {
			err = fmt.Errorf("start hook: %w", hookErr)
		}
	}
	if r.exitRequested {
		r.stopAfterExit(L)
		r.ready <- ErrExited
		return
	}
	r.ready <- err
	if err != nil {
		return
	}

	for {
		select {
		case <-r.ctx.Done():
			return
		case exit := <-r.processExits:
			r.reportProcessExit(L, exit)
			if r.exitRequested {
				r.stopAfterExit(L)
				return
			}
		case fire := <-r.scheduleFires:
			r.handleScheduleFire(L, fire)
			if r.exitRequested {
				r.stopAfterExit(L)
				return
			}
		case cmd := <-r.cmds:
			switch cmd.kind {
			case commandHandle:
				response, err := r.handle(L, cmd.req)
				if r.exitRequested {
					cmd.done <- result{err: ErrStopped}
					r.stopAfterExit(L)
					return
				}
				cmd.done <- result{response: response, err: err}
			case commandStop:
				cmd.done <- result{err: r.stopHook(L)}
				return
			}
		}
	}
}

func (r *Runtime) stopAfterExit(L *glua.LState) {
	execution, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.cancelExecution = cancel
	L.SetContext(execution)
	r.exitRequested = false
	if err := r.stopHook(L); err != nil {
		r.opts.Logger.Error("stop hook after plugin exit", "error", err)
	}
}

func (r *Runtime) stopHook(L *glua.LState) error {
	err := r.callHook(L, "stop")
	if r.exitRequested {
		return nil
	}
	return err
}

func (r *Runtime) loadEntry(L *glua.LState) error {
	fn, err := L.LoadFile(r.opts.EntryPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", r.opts.EntryPath, err)
	}
	if err := L.CallByParam(glua.P{Fn: fn, NRet: 0, Protect: true}); err != nil {
		return fmt.Errorf("run %s: %w", r.opts.EntryPath, err)
	}
	return nil
}

func (r *Runtime) callHook(L *glua.LState, name string) error {
	fn := r.hooks[name]
	if fn == nil {
		return nil
	}
	return L.CallByParam(glua.P{Fn: fn, NRet: 0, Protect: true})
}

func (r *Runtime) handle(L *glua.LState, req Request) (Response, error) {
	key := req.Method + " " + req.Path
	fn := r.routes[key]
	if fn == nil {
		if methods := r.routeMethods[req.Path]; len(methods) > 0 {
			return errorResponse(405, fmt.Sprintf("method %s is not allowed for %s", req.Method, req.Path)), nil
		}
		return errorResponse(404, fmt.Sprintf("no handler for %s %s", req.Method, req.Path)), nil
	}
	requestTable := r.requestTable(L, req)
	if err := L.CallByParam(glua.P{Fn: fn, NRet: 1, Protect: true}, requestTable); err != nil {
		return Response{}, fmt.Errorf("lua handler %s: %w", key, err)
	}
	value := L.Get(-1)
	L.Pop(1)
	return r.parseResponse(L, value)
}

func (r *Runtime) requestTable(L *glua.LState, req Request) *glua.LTable {
	table := L.NewTable()
	table.RawSetString("method", glua.LString(req.Method))
	table.RawSetString("path", glua.LString(req.Path))
	table.RawSetString("body", glua.LString(string(req.Body)))
	table.RawSetString("remoteAddr", glua.LString(req.RemoteAddr))

	query := L.NewTable()
	for name, value := range req.Query {
		query.RawSetString(name, glua.LString(value))
	}
	table.RawSetString("query", query)

	headers := L.NewTable()
	for name, value := range req.Headers {
		headers.RawSetString(name, glua.LString(value))
	}
	table.RawSetString("headers", headers)
	return table
}

func (r *Runtime) parseResponse(L *glua.LState, value glua.LValue) (Response, error) {
	table, ok := value.(*glua.LTable)
	if !ok {
		if value == glua.LNil {
			return Response{}, errors.New("handler returned no response")
		}
		return Response{}, fmt.Errorf("handler must return a table, got %s", value.Type())
	}
	response := Response{Status: 200, Headers: map[string]string{}}
	if status := table.RawGetString("status"); status != glua.LNil {
		number, ok := status.(glua.LNumber)
		if !ok {
			return Response{}, fmt.Errorf("response status must be a number, got %s", status.Type())
		}
		code := int(number)
		if code < 100 || code > 599 {
			return Response{}, fmt.Errorf("response status %d is invalid", code)
		}
		response.Status = code
	}
	if headers := table.RawGetString("headers"); headers != glua.LNil {
		headerTable, ok := headers.(*glua.LTable)
		if !ok {
			return Response{}, fmt.Errorf("response headers must be a table, got %s", headers.Type())
		}
		var conversionErr error
		headerTable.ForEach(func(key, item glua.LValue) {
			if conversionErr != nil {
				return
			}
			name, ok := key.(glua.LString)
			if !ok {
				conversionErr = fmt.Errorf("response header name must be a string, got %s", key.Type())
				return
			}
			switch typed := item.(type) {
			case glua.LString:
				response.Headers[string(name)] = string(typed)
			case *glua.LTable:
				parts := make([]string, 0, typed.Len())
				typed.ForEach(func(_, part glua.LValue) {
					if conversionErr != nil {
						return
					}
					s, ok := part.(glua.LString)
					if !ok {
						conversionErr = fmt.Errorf("response header %q values must be strings", name)
						return
					}
					parts = append(parts, string(s))
				})
				response.Headers[string(name)] = strings.Join(parts, ", ")
			default:
				conversionErr = fmt.Errorf("response header %q must be a string or table", name)
			}
		})
		if conversionErr != nil {
			return Response{}, conversionErr
		}
	}
	if body := table.RawGetString("body"); body != glua.LNil {
		text, ok := body.(glua.LString)
		if !ok {
			return Response{}, fmt.Errorf("response body must be a string, got %s", body.Type())
		}
		response.Body = []byte(text)
	}
	return response, nil
}

func errorResponse(status int, message string) Response {
	body, _ := json.Marshal(map[string]string{"error": message})
	return Response{
		Status:  status,
		Headers: map[string]string{"content-type": "application/json; charset=utf-8"},
		Body:    body,
	}
}

// withinRoot reports whether name stays below root.
func withinRoot(root, name string) bool {
	relative, err := filepath.Rel(root, name)
	if err != nil {
		return false
	}
	return filepath.IsLocal(relative)
}
