// Package lua runs plugin Lua code on a dedicated goroutine per plugin instance.
package lua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/EziosWJ/Extension-Host/internal/schedule"
	"github.com/EziosWJ/Extension-Host/internal/serviceaccess"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

// ErrStopped is returned when the runtime is no longer running.
var ErrStopped = errors.New("plugin instance is stopped")

// ErrExited is returned when the plugin calls os.exit during startup.
var ErrExited = errors.New("plugin exited during startup")

// Request is the request passed to a Lua HTTP handler.
type Request struct {
	Method     string
	Path       string // Decoded URL path relative to the plugin API mount.
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

// Options configures a plugin Lua runtime.
type Options struct {
	EntryPath       string
	PackageRoot     string
	DataDir         string
	Logger          *slog.Logger
	Config          func() map[string]any
	Storage         *store.Business
	Emit            func(event string, data any) error
	SubscriberCount func() int
	Services        *serviceaccess.Access
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
	execCtx      context.Context
	cancelExec   context.CancelFunc

	spawnNotify chan struct{}
	spawnMu     sync.Mutex
	spawnQueue  []*spawnedProcess

	subscriberNotify    chan struct{}
	lastSubscriberCount int

	scheduler     *schedule.Scheduler
	schedules     map[string]*scheduleEntry
	scheduleFires chan scheduleFire

	startMu sync.Mutex
	started bool
	stop    sync.Once

	routes       map[string]*glua.LFunction
	routeMethods map[string]map[string]bool
	paramRoutes  []paramRoute
	hooks        map[string]*glua.LFunction
	modules      map[string]glua.LValue
	loading      map[string]bool

	exitRequested   bool
	cancelExecution context.CancelFunc

	requestEntry       *glua.LFunction
	activeRequests     map[*glua.LState]*pendingHTTPRequest
	asyncYieldSentinel glua.LValue
	asyncCompletions   chan asyncCompletion
}

type paramRoute struct {
	segments    []string
	staticCount int
	handlers    map[string]paramRouteHandler
}

type paramRouteHandler struct {
	fn    *glua.LFunction
	names []string
}

type commandKind int

const (
	commandHandle commandKind = iota
	commandStop
)

type command struct {
	kind commandKind
	ctx  context.Context
	req  Request
	done chan result
}

type pendingHTTPRequest struct {
	thread *glua.LState
	ctx    context.Context
	opCtx  context.Context
	cancel context.CancelFunc
	done   chan result
	key    string
}

type asyncCompletion struct {
	req    *pendingHTTPRequest
	values luaResults
}

type luaResults func(*glua.LState) []glua.LValue

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
	execCtx, cancelExec := context.WithCancel(ctx)
	return &Runtime{
		opts:             opts,
		entryDir:         filepath.Dir(opts.EntryPath),
		ctx:              ctx,
		cancel:           cancel,
		cmds:             make(chan command),
		ready:            make(chan error, 1),
		done:             make(chan struct{}),
		processExits:     make(chan processExit),
		spawnNotify:      make(chan struct{}, 1),
		subscriberNotify: make(chan struct{}, 1),
		execCtx:          execCtx,
		cancelExec:       cancelExec,
		schedules:        make(map[string]*scheduleEntry),
		scheduleFires:    make(chan scheduleFire),
		routes:           make(map[string]*glua.LFunction),
		routeMethods:     make(map[string]map[string]bool),
		hooks:            make(map[string]*glua.LFunction),
		modules:          make(map[string]glua.LValue),
		loading:          make(map[string]bool),
		activeRequests:   make(map[*glua.LState]*pendingHTTPRequest),
		asyncCompletions: make(chan asyncCompletion),
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
	case r.cmds <- command{kind: commandHandle, ctx: ctx, req: req, done: reply}:
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
		// A synchronous command can be holding the Lua goroutine that
		// receives the stop request.
		r.cancelExec()
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

// NotifySubscribers wakes the runtime loop after the active subscriber count changes.
func (r *Runtime) NotifySubscribers() {
	select {
	case r.subscriberNotify <- struct{}{}:
	default:
	}
}

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
	defer r.stopRequests()
	execution, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.cancelExecution = cancel
	L.SetContext(execution)

	r.scheduler = schedule.New(r.opts.Logger)

	// Keep a Lua frame below the handler, even if it tail-calls a Go
	// function. GopherLua treats a yield with an empty stack as a return.
	entry, err := L.LoadString("local handler, request = ...; local response = handler(request); return response")
	if err != nil {
		r.ready <- err
		return
	}
	r.requestEntry = entry
	r.asyncYieldSentinel = L.NewUserData()

	if err := r.setup(L); err != nil {
		r.ready <- err
		return
	}
	r.guardRequestThreads(L)
	err = r.loadEntry(L)
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
		case <-r.spawnNotify:
			r.handleSpawnEvent(L)
		case <-r.subscriberNotify:
			r.handleSubscribersChange(L)
		case fire := <-r.scheduleFires:
			r.handleScheduleFire(L, fire)
		case completion := <-r.asyncCompletions:
			r.completeAsync(L, completion)
		case cmd := <-r.cmds:
			switch cmd.kind {
			case commandHandle:
				r.handle(L, cmd)
			case commandStop:
				r.stopRequests()
				cmd.done <- result{err: r.stopHook(L)}
				return
			}
		}
		if r.exitRequested {
			r.stopAfterExit(L)
			return
		}
	}
}

func (r *Runtime) stopAfterExit(L *glua.LState) {
	r.cancelExec()
	r.stopRequests()
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

func (r *Runtime) handleSubscribersChange(L *glua.LState) {
	if r.opts.SubscriberCount == nil {
		return
	}
	count := r.opts.SubscriberCount()
	if count == r.lastSubscriberCount {
		return
	}
	r.lastSubscriberCount = count
	fn := r.hooks["subscribers"]
	if fn == nil {
		return
	}
	if err := L.CallByParam(glua.P{Fn: fn, NRet: 0, Protect: true}, glua.LNumber(count)); err != nil && !r.exitRequested {
		r.opts.Logger.Error("subscribers hook failed", "count", count, "error", err)
	}
}

func (r *Runtime) handle(L *glua.LState, cmd command) {
	if err := cmd.ctx.Err(); err != nil {
		cmd.done <- result{err: err}
		return
	}
	req := cmd.req
	key := req.Method + " " + req.Path
	fn := r.routes[key]
	pathMatched := len(r.routeMethods[req.Path]) > 0
	var params map[string]string
	if !pathMatched && len(r.paramRoutes) > 0 {
		fn, params, pathMatched = r.matchParamRoute(req.Method, req.Path)
	}
	if fn == nil {
		if pathMatched {
			cmd.done <- result{response: errorResponse(405, fmt.Sprintf("method %s is not allowed for %s", req.Method, req.Path))}
		} else {
			cmd.done <- result{response: errorResponse(404, fmt.Sprintf("no handler for %s %s", req.Method, req.Path))}
		}
		return
	}
	thread, cancelThread := L.NewThread()
	cancelThread()
	thread.SetContext(L.Context())
	opCtx, cancelOp := context.WithCancel(r.execCtx)
	stopCancel := context.AfterFunc(cmd.ctx, cancelOp)
	pending := &pendingHTTPRequest{
		thread: thread,
		ctx:    cmd.ctx,
		opCtx:  opCtx,
		cancel: func() {
			stopCancel()
			cancelOp()
		},
		done: cmd.done,
		key:  key,
	}
	r.activeRequests[thread] = pending
	r.resumeRequest(L, pending, r.requestEntry, fn, r.requestTable(L, req, params))
}

func (r *Runtime) matchParamRoute(method, routePath string) (*glua.LFunction, map[string]string, bool) {
	path, absolute := strings.CutPrefix(routePath, "/")
	if !absolute {
		return nil, nil, false
	}
	segments := strings.Split(path, "/")
	var best *paramRoute
	for i := range r.paramRoutes {
		route := &r.paramRoutes[i]
		if route.matches(segments) && (best == nil || route.moreSpecificThan(best)) {
			best = route
		}
	}
	if best == nil {
		return nil, nil, false
	}
	handler, ok := best.handlers[method]
	if !ok {
		return nil, nil, true
	}
	params := make(map[string]string, len(segments)-best.staticCount)
	for i, name := range handler.names {
		if name != "" {
			params[name] = segments[i]
		}
	}
	return handler.fn, params, true
}

func (route *paramRoute) matches(segments []string) bool {
	if len(segments) != len(route.segments) {
		return false
	}
	for i, literal := range route.segments {
		if segments[i] == "" || (literal != "" && literal != segments[i]) {
			return false
		}
	}
	return true
}

func (route *paramRoute) moreSpecificThan(other *paramRoute) bool {
	if route.staticCount != other.staticCount {
		return route.staticCount > other.staticCount
	}
	for i, literal := range route.segments {
		if (literal == "") != (other.segments[i] == "") {
			return literal != ""
		}
	}
	return false
}

func (r *Runtime) resumeRequest(L *glua.LState, req *pendingHTTPRequest, fn *glua.LFunction, args ...glua.LValue) {
	state, err, values := L.Resume(req.thread, fn, args...)
	if r.exitRequested {
		return
	}
	switch state {
	case glua.ResumeOK:
		response, err := r.parseResponse(L, values[0])
		r.finishRequest(req, result{response: response, err: err})
	case glua.ResumeError:
		r.finishRequest(req, result{err: fmt.Errorf("lua handler %s: %w", req.key, err)})
	case glua.ResumeYield:
		if len(values) == 0 || values[0] != r.asyncYieldSentinel {
			r.finishRequest(req, result{err: fmt.Errorf("lua handler %s: unexpected coroutine yield", req.key)})
		}
	}
}

func (r *Runtime) finishRequest(req *pendingHTTPRequest, res result) {
	delete(r.activeRequests, req.thread)
	req.thread.Dead = true
	req.cancel()
	req.done <- res
}

func (r *Runtime) stopRequests() {
	for _, req := range r.activeRequests {
		r.finishRequest(req, result{err: ErrStopped})
	}
}

func (r *Runtime) guardRequestThreads(L *glua.LState) {
	coroutine := L.GetGlobal("coroutine").(*glua.LTable)
	resume := coroutine.RawGetString("resume").(*glua.LFunction)
	coroutine.RawSetString("resume", L.NewFunction(func(L *glua.LState) int {
		// A handler can share coroutine.running() with other Lua code.
		// Only the runtime can resume it with the pending operation's results.
		if thread, ok := L.Get(1).(*glua.LState); ok && r.activeRequests[thread] != nil {
			L.Push(glua.LFalse)
			L.Push(glua.LString("cannot resume an HTTP handler coroutine"))
			return 2
		}
		return resume.GFunction(L)
	}))
}

func (r *Runtime) canYieldRequest(L *glua.LState) (*pendingHTTPRequest, bool) {
	req := r.activeRequests[L]
	if req == nil || L.Parent == nil {
		return nil, false
	}
	// Yielding through a Go call such as pcall leaves its Lua stack unwound
	// while its Go code still expects the call to return normally.
	for level := 1; ; level++ {
		dbg, ok := L.GetStack(level)
		if !ok {
			break
		}
		if _, err := L.GetInfo("S", dbg, nil); err != nil || dbg.What == "G" {
			return nil, false
		}
	}
	if !canYieldThroughVM() {
		return nil, false
	}
	return req, true
}

func canYieldThroughVM() bool {
	// GopherLua's synchronous calls into Lua (including metamethods and
	// generic for iterators) can enter callR without adding a Go Lua frame.
	// A yield from there returns to callR instead of unwinding to Resume.
	const state = "github.com/yuin/gopher-lua.(*LState)."
	var stack [32]uintptr
	n := runtime.Callers(2, stack[:])
	frames := runtime.CallersFrames(stack[:n])
	for {
		frame, more := frames.Next()
		switch frame.Function {
		case state + "callR":
			return false
		case state + "Resume":
			return true
		}
		if !more {
			return false
		}
	}
}

func (r *Runtime) operationContext(L *glua.LState) context.Context {
	if req := r.activeRequests[L]; req != nil {
		return req.opCtx
	}
	return r.execCtx
}

func (r *Runtime) startAsync(req *pendingHTTPRequest, work func() luaResults) {
	r.processes.Add(1)
	go func() {
		defer r.processes.Done()
		values := work()
		select {
		case r.asyncCompletions <- asyncCompletion{req: req, values: values}:
		case <-r.ctx.Done():
		}
	}()
}

func (r *Runtime) completeAsync(L *glua.LState, completion asyncCompletion) {
	req := completion.req
	if r.activeRequests[req.thread] != req {
		return
	}
	if err := req.ctx.Err(); err != nil {
		r.finishRequest(req, result{err: err})
		return
	}
	if r.execCtx.Err() != nil {
		r.finishRequest(req, result{err: ErrStopped})
		return
	}
	r.resumeRequest(L, req, nil, completion.values(L)...)
}

func pushResults(L *glua.LState, values []glua.LValue) int {
	for _, value := range values {
		L.Push(value)
	}
	return len(values)
}

func (r *Runtime) requestTable(L *glua.LState, req Request, params map[string]string) *glua.LTable {
	table := L.NewTable()
	table.RawSetString("method", glua.LString(req.Method))
	table.RawSetString("path", glua.LString(req.Path))
	table.RawSetString("body", glua.LString(string(req.Body)))
	table.RawSetString("remoteAddr", glua.LString(req.RemoteAddr))

	paramTable := L.NewTable()
	for name, value := range params {
		paramTable.RawSetString(name, glua.LString(value))
	}
	table.RawSetString("params", paramTable)

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
