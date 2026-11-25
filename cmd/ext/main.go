// Command ext runs the standalone GoDesk Extension Host.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/internal/bundle"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "ext:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return serve(ctx, args)
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:])
	case "run":
		return runPlugin(ctx, args[1:])
	case "version":
		fmt.Fprintln(os.Stdout, host.Version)
		return nil
	case "validate":
		return validate(args[1:])
	case "pack":
		return pack(args[1:])
	case "init":
		return initProject(args[1:], os.Stdin, os.Stdout)
	case "dev":
		return develop(ctx, args[1:])
	case "pkg":
		return packageProject(args[1:])
	case "help":
		usage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q; run ext help", args[0])
	}
}

func usage(out *os.File) {
	fmt.Fprintf(out, `ext - standalone GoDesk Extension Host

Usage:
  ext [flags]                  Start the host and management page
  ext serve [flags]            Start the host and management page
  ext run <dir|zip> [flags]    Install, enable and serve one plugin
  ext version                 Print the Extension Host version
  ext validate <dir|zip>       Validate a plugin package
  ext pack <dir> [-o file]     Build a plugin ZIP
  ext init                     Create a named plugin project folder
  ext dev [flags]              Run the current project and reload on save
  ext pkg                      Build the current project into dist/

Serve, run and dev flags:
  --listen string          HTTP listen address (default "127.0.0.1:5001")
  --service-grants string  JSON file granting plugins access to systemd units

Serve and run only:
  --data-dir string        host data directory (default "./ext-data")

Examples:
  ext init
  ext dev
  ext pkg
  ext pack ./examples/hello -o dist/hello.zip
`)
}

func serve(ctx context.Context, args []string) error {
	listen := "127.0.0.1:5001"
	dataDir := "./ext-data"
	grantsFile := ""
	positional, err := parseFlags(args, []stringFlag{
		{names: []string{"listen"}, value: &listen},
		{names: []string{"data-dir"}, value: &dataDir},
		{names: []string{"service-grants"}, value: &grantsFile},
	})
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("serve accepts no positional arguments")
	}
	grants, err := host.LoadServiceGrantsFile(grantsFile)
	if err != nil {
		return err
	}

	extensionHost, err := host.Open(ctx, host.Options{DataDir: dataDir, ServiceGrants: grants})
	if err != nil {
		return err
	}
	defer closeHost(extensionHost)
	if err := extensionHost.Restore(ctx); err != nil {
		slog.Error("restore enabled plugins", "error", err)
	}
	return serveHost(ctx, extensionHost, listen, dataDir, nil)
}

func runPlugin(ctx context.Context, args []string) error {
	listen := "127.0.0.1:5001"
	dataDir := "./ext-data"
	grantsFile := ""
	positional, err := parseFlags(args, []stringFlag{
		{names: []string{"listen"}, value: &listen},
		{names: []string{"data-dir"}, value: &dataDir},
		{names: []string{"service-grants"}, value: &grantsFile},
	})
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("run requires one package path (directory or ZIP)")
	}
	pkg, err := loadBundle(positional[0])
	if err != nil {
		return err
	}
	if err := pkg.Manifest.CheckCompatibility(); err != nil {
		return err
	}
	grants, err := host.LoadServiceGrantsFile(grantsFile)
	if err != nil {
		return err
	}

	extensionHost, err := host.Open(ctx, host.Options{DataDir: dataDir, ServiceGrants: grants})
	if err != nil {
		return err
	}
	defer closeHost(extensionHost)

	installed, err := extensionHost.List(ctx)
	if err != nil {
		return err
	}
	for _, plugin := range installed {
		if plugin.ID == pkg.Manifest.ID {
			if err := extensionHost.Uninstall(ctx, plugin.ID, host.UninstallOptions{}); err != nil {
				return fmt.Errorf("replace existing plugin: %w", err)
			}
		}
	}
	if err := extensionHost.Restore(ctx); err != nil {
		slog.Error("restore enabled plugins", "error", err)
	}

	archive, err := pkg.Zip()
	if err != nil {
		return err
	}
	plugin, err := extensionHost.Install(ctx, bytes.NewReader(archive))
	if err != nil {
		return err
	}
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		return fmt.Errorf("enable %s: %w", plugin.ID, err)
	}
	slog.Info("plugin enabled", "plugin", plugin.ID, "version", plugin.Version)
	if plugin.HasWeb {
		slog.Info("plugin web entry", "url", "http://"+displayAddress(listen)+plugin.WebPath)
	}
	if plugin.APIPath != "" {
		slog.Info("plugin API entry", "url", "http://"+displayAddress(listen)+plugin.APIPath)
	}
	return serveHost(ctx, extensionHost, listen, dataDir, nil)
}

func serveHost(ctx context.Context, extensionHost *host.Host, listen, dataDir string, dev *developmentSession) error {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	server := &http.Server{
		Handler:           standaloneHandler(extensionHost, dev),
		ReadHeaderTimeout: 5 * time.Second,
	}
	stopWatch := func() {}
	if dev != nil {
		stopWatch = dev.watch(ctx)
	}
	defer stopWatch()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	slog.Info("ext listening", "url", "http://"+displayAddress(listener.Addr().String())+"/", "dataDir", dataDir)

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	stopWatch()
	closeErr := closeHost(extensionHost)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	serveResult := <-serveErr
	if serveResult != nil && !errors.Is(serveResult, http.ErrServerClosed) {
		shutdownErr = errors.Join(shutdownErr, serveResult)
	}
	return errors.Join(closeErr, shutdownErr)
}

func validate(args []string) error {
	positional, err := parseFlags(args, nil)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("validate requires one package path (directory or ZIP)")
	}
	pkg, err := loadBundle(positional[0])
	if err != nil {
		return err
	}
	var entries []string
	if pkg.Manifest.Entry.Web != "" {
		entries = append(entries, "web="+pkg.Manifest.Entry.Web)
	}
	if pkg.Manifest.Entry.Lua != "" {
		entries = append(entries, "lua="+pkg.Manifest.Entry.Lua)
	}
	if pkg.Manifest.Native != nil {
		entries = append(entries, "native="+pkg.Manifest.Entry.Native)
		fmt.Printf("platform: %s\n", pkg.Manifest.Native.Platform)
	}
	if pkg.Manifest.MinHostVersion != "" {
		fmt.Printf("minimum Extension Host: %s\n", pkg.Manifest.MinHostVersion)
	}
	fmt.Printf("plugin: %s (%s) %s\n", pkg.Manifest.Name, pkg.Manifest.ID, pkg.Manifest.Version)
	fmt.Printf("host api: %d-%d\n", pkg.Manifest.HostAPI.Min, pkg.Manifest.HostAPI.Max)
	fmt.Printf("entries: %s\n", strings.Join(entries, ", "))
	if len(pkg.Manifest.Capabilities) > 0 {
		fmt.Printf("capabilities: %s\n", strings.Join(pkg.Manifest.Capabilities, ", "))
	}
	fmt.Printf("files: %d\n", len(pkg.Files))
	fmt.Println("OK: manifest, entries and package paths are valid")
	return nil
}

func pack(args []string) error {
	output := ""
	positional, err := parseFlags(args, []stringFlag{
		{names: []string{"o", "output"}, value: &output},
	})
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("pack requires one source directory")
	}
	source, err := filepath.Abs(positional[0])
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("open package directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("pack requires a source directory, got %s", positional[0])
	}
	pkg, err := bundle.LoadDir(source)
	if err != nil {
		return err
	}
	target := output
	if target == "" {
		target = filepath.Join("dist", pkg.Filename())
	}
	absoluteTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absoluteTarget); err == nil && info.IsDir() {
		return fmt.Errorf("output path %s is a directory", target)
	}
	// Exclude a previous output file that lives inside the source directory.
	pkg, err = bundle.LoadDir(source, absoluteTarget)
	if err != nil {
		return err
	}
	return writePackage(pkg, absoluteTarget)
}

func writePackage(pkg *bundle.Bundle, target string) error {
	archive, err := pkg.Zip()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".pack-*.zip")
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(archive); err != nil {
		temporary.Close()
		return fmt.Errorf("write output file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write output file: %w", err)
	}
	if err := os.Chmod(temporaryName, 0o644); err != nil {
		return fmt.Errorf("set output permissions: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("publish output file: %w", err)
	}
	fmt.Printf("packed %s (%d bytes)\n", target, len(archive))
	return nil
}

type stringFlag struct {
	names []string
	value *string
}

// parseFlags accepts flags before or after positional arguments.
func parseFlags(args []string, flags []stringFlag) ([]string, error) {
	var positional []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "-h" || arg == "--help" {
			usage(os.Stdout)
			return nil, flag.ErrHelp
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		value := ""
		hasValue := false
		if before, after, found := strings.Cut(name, "="); found {
			name, value, hasValue = before, after, true
		}
		var target *string
		for index := range flags {
			for _, candidate := range flags[index].names {
				if candidate == name {
					target = flags[index].value
				}
			}
		}
		if target == nil {
			return nil, fmt.Errorf("unknown flag %q", arg)
		}
		if !hasValue {
			index++
			if index >= len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			value = args[index]
		}
		*target = value
	}
	return positional, nil
}

func loadBundle(name string) (*bundle.Bundle, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("open package: %w", err)
	}
	if info.IsDir() {
		return bundle.LoadDir(name)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("package path %s is not a regular file or directory", name)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read package: %w", err)
	}
	return bundle.LoadZip(data)
}

func closeHost(extensionHost *host.Host) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return extensionHost.Close(ctx)
}

func displayAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
