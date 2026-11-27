package host_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestLuaSubscriberObservabilityStopsStreamOnDisconnect(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	server := startServer(t, extensionHost)

	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.subscribers", "name": "Subscribers test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		local history = host.json.decode("[]")
		local active = nil
		local exited = nil

		host.app.on("subscribers", function(count)
			table.insert(history, count)
			if count == 0 and active and active:running() then
				active:stop(0)
			end
		end)

		host.app.route("GET", "/state", function()
			return {
				headers = { ["content-type"] = "application/json; charset=utf-8" },
				body = host.json.encode({
					count = host.app.subscriberCount(),
					history = history,
					running = active ~= nil and active:running() or false,
					pid = active and active:pid() or 0,
					exited = exited,
				}),
			}
		end)

		host.app.route("POST", "/stream", function()
			local h, err = host.process.spawn("sh", {"-c", "printf ready; sleep 30"}, {
				onStdout = function(chunk)
					host.app.emit("chunk", { data = chunk })
				end,
				onExit = function(res)
					exited = res
				end,
			})
			if not h then
				return { status = 500, body = err }
			end
			active = h
			return { body = tostring(h:pid()) }
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + plugin.EventsPath
	type stateSnapshot struct {
		Count   int   `json:"count"`
		History []int `json:"history"`
		Running bool  `json:"running"`
		PID     int   `json:"pid"`
		Exited  *struct {
			PID     int  `json:"pid"`
			Stopped bool `json:"stopped"`
		} `json:"exited"`
	}
	getState := func() stateSnapshot {
		t.Helper()
		status, body := httpGet(t, server.URL+plugin.APIPath+"state")
		if status != http.StatusOK {
			t.Fatalf("GET state = %d, %q", status, body)
		}
		var snap stateSnapshot
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("decode state %q: %v", body, err)
		}
		return snap
	}
	waitFor := func(desc string, pred func(stateSnapshot) bool) stateSnapshot {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			snap := getState()
			if pred(snap) {
				return snap
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; last state = %+v", desc, snap)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if snap := getState(); snap.Count != 0 {
		t.Fatalf("initial subscriberCount = %d, want 0", snap.Count)
	}

	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws1: %v", err)
	}
	defer conn1.CloseNow()

	waitFor("count == 1", func(s stateSnapshot) bool {
		return s.Count == 1 && len(s.History) >= 1 && s.History[len(s.History)-1] == 1
	})

	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws2: %v", err)
	}
	defer conn2.CloseNow()

	waitFor("count == 2", func(s stateSnapshot) bool {
		return s.Count == 2 && len(s.History) >= 2 && s.History[len(s.History)-1] == 2
	})

	// Start a spawned stream while subscribers are connected.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+plugin.APIPath+"stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST stream = %d", resp.StatusCode)
	}

	// Receive the initial streamed chunk on conn1.
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, msg, err := conn1.Read(readCtx)
	if err != nil {
		t.Fatalf("read chunk event: %v", err)
	}
	if !strings.Contains(string(msg), `"ready"`) {
		t.Fatalf("unexpected chunk event: %s", msg)
	}

	// Closing conn2 drops subscriberCount to 1, stream must keep running.
	_ = conn2.Close(websocket.StatusNormalClosure, "bye")
	snap := waitFor("count == 1 after ws2 close", func(s stateSnapshot) bool {
		return s.Count == 1 && s.History[len(s.History)-1] == 1
	})
	if !snap.Running {
		t.Fatal("stream stopped prematurely while one subscriber was still connected")
	}

	// Closing conn1 drops subscriberCount to 0 -> hook stops spawned process automatically.
	_ = conn1.Close(websocket.StatusNormalClosure, "bye")
	snap = waitFor("count == 0 and stream stopped", func(s stateSnapshot) bool {
		return s.Count == 0 && !s.Running && s.Exited != nil && s.Exited.Stopped
	})
	if snap.PID <= 0 {
		t.Fatalf("expected valid pid, got %d", snap.PID)
	}
}

func TestLuaEventsBurst128WithoutDrops(t *testing.T) {
	ctx := testContext(t)
	extensionHost, _ := newTestHost(t, ctx)
	server := startServer(t, extensionHost)

	plugin := installPackage(t, ctx, extensionHost, `{
		"schemaVersion": 1, "id": "test.lua.burst", "name": "Burst test",
		"version": "1.0.0", "hostApi": {"min": 1, "max": 1},
		"entry": {"lua": "init.lua"}
	}`, map[string]string{"init.lua": `
		host.app.route("POST", "/burst", function()
			for i = 1, 128 do
				host.app.emit("seq", { n = i })
			end
			return { body = "ok" }
		end)
	`})
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + plugin.EventsPath
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.CloseNow()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+plugin.APIPath+"burst", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST burst = %d", resp.StatusCode)
	}

	for i := 1; i <= 128; i++ {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, raw, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			t.Fatalf("read event %d: %v", i, err)
		}
		var env struct {
			Event string `json:"event"`
			Data  struct {
				N int `json:"n"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode event %d (%q): %v", i, raw, err)
		}
		if env.Event != "seq" || env.Data.N != i {
			t.Fatalf("event %d = %+v, want n=%s", i, env, strconv.Itoa(i))
		}
	}
}
