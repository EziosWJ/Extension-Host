package lua

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

type httpRequestSpec struct {
	url                string
	method             string
	headers            http.Header
	body               string
	timeout            time.Duration
	insecureSkipVerify bool
	followRedirects    bool
	maxResponseBytes   int
}

type httpResponseData struct {
	status        int
	headers       map[string]string
	body          []byte
	bodyTruncated bool
	latencyMs     int64
}

func (r *Runtime) httpModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("request", L.NewFunction(r.httpRequest))
	return module
}

func (r *Runtime) httpRequest(L *glua.LState) int {
	spec := parseHTTPRequest(L)
	if req, ok := r.canYieldRequest(L); ok {
		r.startAsync(req, func() luaResults {
			response, err := r.performHTTPRequest(req.opCtx, spec)
			return func(L *glua.LState) []glua.LValue {
				return httpRequestValues(L, response, err)
			}
		})
		return L.Yield(r.asyncYieldSentinel)
	}
	response, err := r.performHTTPRequest(r.operationContext(L), spec)
	return pushResults(L, httpRequestValues(L, response, err))
}

func httpRequestValues(L *glua.LState, response *httpResponseData, err error) []glua.LValue {
	if err != nil {
		return []glua.LValue{glua.LNil, glua.LString(err.Error())}
	}

	headers := L.NewTable()
	for name, value := range response.headers {
		headers.RawSetString(name, glua.LString(value))
	}
	result := L.NewTable()
	result.RawSetString("status", glua.LNumber(response.status))
	result.RawSetString("headers", headers)
	result.RawSetString("body", glua.LString(response.body))
	result.RawSetString("bodyTruncated", glua.LBool(response.bodyTruncated))
	result.RawSetString("latencyMs", glua.LNumber(response.latencyMs))
	return []glua.LValue{result}
}

func parseHTTPRequest(L *glua.LState) httpRequestSpec {
	options, ok := L.Get(1).(*glua.LTable)
	if !ok {
		L.ArgError(1, "options must be a table")
	}

	address := httpStringOption(L, options, "url", "")
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		L.ArgError(1, "url must be an absolute HTTP or HTTPS URL with a host")
	}
	method := strings.ToUpper(httpStringOption(L, options, "method", http.MethodGet))
	if !validMethod(method) {
		L.ArgError(1, "invalid method")
	}

	headers := make(http.Header)
	if value := options.RawGetString("headers"); value != glua.LNil {
		table, ok := value.(*glua.LTable)
		if !ok {
			L.ArgError(1, "headers must be a table")
		}
		table.ForEach(func(key, value glua.LValue) {
			name, validName := key.(glua.LString)
			text, validValue := value.(glua.LString)
			if !validName || name == "" || !validValue {
				L.ArgError(1, "headers must have non-empty string keys and string values")
			}
			headers.Add(string(name), string(text))
		})
	}

	return httpRequestSpec{
		url:                address,
		method:             method,
		headers:            headers,
		body:               httpStringOption(L, options, "body", ""),
		timeout:            time.Duration(httpIntOption(L, options, "timeoutMs", 15000, 120000)) * time.Millisecond,
		insecureSkipVerify: httpBoolOption(L, options, "insecureSkipVerify", false),
		followRedirects:    httpBoolOption(L, options, "followRedirects", true),
		maxResponseBytes:   httpIntOption(L, options, "maxResponseBytes", 1<<20, 8<<20),
	}
}

func httpStringOption(L *glua.LState, options *glua.LTable, name, fallback string) string {
	value := options.RawGetString(name)
	if value == glua.LNil {
		return fallback
	}
	result, ok := value.(glua.LString)
	if !ok {
		L.ArgError(1, name+" must be a string")
	}
	return string(result)
}

func httpBoolOption(L *glua.LState, options *glua.LTable, name string, fallback bool) bool {
	value := options.RawGetString(name)
	if value == glua.LNil {
		return fallback
	}
	result, ok := value.(glua.LBool)
	if !ok {
		L.ArgError(1, name+" must be a boolean")
	}
	return bool(result)
}

func httpIntOption(L *glua.LState, options *glua.LTable, name string, fallback, max int) int {
	value := options.RawGetString(name)
	if value == glua.LNil {
		return fallback
	}
	number, ok := value.(glua.LNumber)
	if !ok || number < 1 || number > glua.LNumber(max) || math.Trunc(float64(number)) != float64(number) {
		L.ArgError(1, fmt.Sprintf("%s must be an integer between 1 and %d", name, max))
	}
	return int(number)
}

func (r *Runtime) performHTTPRequest(ctx context.Context, spec httpRequestSpec) (*httpResponseData, error) {
	if ctx.Err() != nil {
		return nil, ErrStopped
	}
	ctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, spec.method, spec.url, strings.NewReader(spec.body))
	if err != nil {
		return nil, err
	}
	request.Header = spec.headers
	if host := spec.headers.Get("Host"); host != "" {
		request.Host = host
	}

	transport := &http.Transport{ForceAttemptHTTP2: true}
	if spec.insecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}
	if !spec.followRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, int64(spec.maxResponseBytes)+1))
	if err != nil {
		return nil, err
	}
	latencyMs := time.Since(start).Milliseconds()
	truncated := len(body) > spec.maxResponseBytes
	if truncated {
		body = body[:spec.maxResponseBytes]
	}
	headers := make(map[string]string, len(response.Header))
	for name, values := range response.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	return &httpResponseData{
		status:        response.StatusCode,
		headers:       headers,
		body:          body,
		bodyTruncated: truncated,
		latencyMs:     latencyMs,
	}, nil
}
