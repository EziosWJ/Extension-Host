package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/internal/bundle"
)

type developmentSession struct {
	host     *host.Host
	id       string
	project  string
	observed map[string]fileStamp
	applied  map[string][sha256.Size]byte
	revision atomic.Uint64
}

type fileStamp struct {
	mode     fs.FileMode
	size     int64
	modified time.Time
}

func develop(ctx context.Context, args []string) error {
	listen := "127.0.0.1:5001"
	positional, err := parseFlags(args, []stringFlag{
		{names: []string{"listen"}, value: &listen},
	})
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("dev uses the current project directory")
	}
	project, err := os.Getwd()
	if err != nil {
		return err
	}
	stamp, err := projectStamp(project)
	if err != nil {
		return err
	}
	pkg, err := bundle.LoadProject(project)
	if err != nil {
		return err
	}
	dataDir := filepath.Join(project, ".ext")
	extensionHost, err := host.Open(ctx, host.Options{DataDir: dataDir})
	if err != nil {
		return err
	}
	defer closeHost(extensionHost)

	dev := &developmentSession{host: extensionHost, id: pkg.Manifest.ID, project: project, observed: stamp}
	dev.revision.Store(uint64(time.Now().UnixNano()))
	_, previouslyInstalled := extensionHost.Get(dev.id)
	dev.apply(ctx, pkg)
	if dev.applied == nil && previouslyInstalled {
		// On process restart, a bad edit can still leave a working installed snapshot.
		startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := extensionHost.Enable(startCtx, dev.id)
		cancel()
		if err != nil {
			slog.Error("could not start the installed development plugin", "error", err)
		}
	}
	if pkg.Manifest.Entry.Lua != "" || (pkg.Manifest.Native != nil && pkg.Manifest.Native.WebAPI) {
		slog.Info("development plugin API", "path", "/extensions/"+dev.id+"/api/")
	}
	return serveHost(ctx, extensionHost, listen, dataDir, dev)
}

func (d *developmentSession) watch(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var pending map[string]fileStamp
		lastError := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			stamp, err := projectStamp(d.project)
			if err != nil {
				if err.Error() != lastError {
					slog.Error("watch plugin project", "error", err)
				}
				lastError = err.Error()
				pending = nil
				continue
			}
			lastError = ""
			if maps.Equal(stamp, d.observed) {
				pending = nil
				continue
			}
			// Wait for one quiet interval so multi-file writes can finish.
			if !maps.Equal(stamp, pending) {
				pending = stamp
				continue
			}
			d.observed = stamp
			pending = nil
			pkg, err := bundle.LoadProject(d.project)
			if err != nil {
				slog.Error("development update failed; save to retry", "error", err)
				continue
			}
			d.apply(ctx, pkg)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (d *developmentSession) apply(ctx context.Context, pkg *bundle.Bundle) {
	if pkg.Manifest.ID != d.id {
		slog.Error("development plugin ID changed; restart ext dev", "plugin", pkg.Manifest.ID)
		return
	}
	files := make(map[string][sha256.Size]byte, len(pkg.Files))
	for path, content := range pkg.Files {
		files[path] = sha256.Sum256(content)
	}
	if maps.Equal(d.applied, files) {
		return
	}
	webOnly := d.applied != nil && sameNonWebFiles(d.applied, files)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.publish(ctx, pkg, webOnly); err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("development update failed; save to retry", "plugin", d.id, "error", err)
		}
		return
	}
	d.applied = files
	d.revision.Add(1)
	slog.Info("development plugin ready", "plugin", d.id, "version", pkg.Manifest.Version)
}

func (d *developmentSession) publish(ctx context.Context, pkg *bundle.Bundle, webOnly bool) error {
	if err := checkLuaSyntax(pkg); err != nil {
		return err
	}
	err := d.host.ReloadDevelopment(ctx, pkg, webOnly)
	if !errors.Is(err, host.ErrNotFound) {
		return err
	}
	archive, err := pkg.Zip()
	if err != nil {
		return err
	}
	if _, err := d.host.Install(ctx, bytes.NewReader(archive)); err != nil {
		return err
	}
	if err := d.host.Enable(ctx, d.id); err != nil {
		return err
	}
	if plugin, ok := d.host.Get(d.id); !ok || plugin.Status != host.StatusRunning {
		return fmt.Errorf("plugin %s exited while starting", d.id)
	}
	return nil
}

func checkLuaSyntax(pkg *bundle.Bundle) error {
	if pkg.Manifest.Entry.Lua == "" {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(pkg.Files)) {
		if name != pkg.Manifest.Entry.Lua && !(strings.HasPrefix(name, "lua/") && filepath.Ext(name) == ".lua") {
			continue
		}
		chunk, err := parse.Parse(bytes.NewReader(pkg.Files[name]), name)
		if err != nil {
			return err
		}
		if _, err := glua.Compile(chunk, name); err != nil {
			return err
		}
	}
	return nil
}

func sameNonWebFiles(previous, next map[string][sha256.Size]byte) bool {
	for name, data := range previous {
		if strings.HasPrefix(name, "web/") {
			continue
		}
		if candidate, ok := next[name]; !ok || data != candidate {
			return false
		}
	}
	for name := range next {
		if _, ok := previous[name]; !ok && !strings.HasPrefix(name, "web/") {
			return false
		}
	}
	return true
}

func projectStamp(project string) (map[string]fileStamp, error) {
	stamp := make(map[string]fileStamp)
	for _, name := range []string{"manifest.json", "web", "lua", "native"} {
		root := filepath.Join(project, name)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			stamp[path] = fileStamp{info.Mode(), info.Size(), info.ModTime()}
			if name == "manifest.json" && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return stamp, nil
}

func (d *developmentSession) revisionFor(id string) string {
	if id != d.id {
		return ""
	}
	return strconv.FormatUint(d.revision.Load(), 10)
}

func (d *developmentSession) serveRevision(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprint(w, d.revisionFor(d.id))
}

//go:embed reload.js
var reloadScript []byte

func serveReloadScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(reloadScript)
}
