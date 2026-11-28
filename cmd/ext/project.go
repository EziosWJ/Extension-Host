package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	texttemplate "text/template"
	"unicode"

	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/manifest"
)

//go:embed templates/*
var projectTemplates embed.FS

type projectType struct {
	label  string
	web    bool
	lua    bool
	native bool
}

var projectTypes = []projectType{
	{label: "网页 + Lua", web: true, lua: true},
	{label: "只有网页", web: true},
	{label: "只有 Lua", lua: true},
	{label: "网页 + 原生后端", web: true, native: true},
	{label: "只有原生后端", native: true},
}

func initProject(args []string, in io.Reader, out io.Writer) error {
	positionals, err := parseFlags(args, nil)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return errors.New("init creates a new project folder under the current directory")
	}

	input := bufio.NewReader(in)
	read := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := input.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprintln(out, "选择项目模板")
	for index, kind := range projectTypes {
		fmt.Fprintf(out, "  %d. %s\n", index+1, kind.label)
	}
	option, err := read("模板 [1]：")
	if err != nil {
		return err
	}
	if option == "" {
		option = "1"
	}
	index, err := strconv.Atoi(option)
	if err != nil || index < 1 || index > len(projectTypes) {
		return fmt.Errorf("请选择模板 1 到 %d", len(projectTypes))
	}
	name, err := read("插件名称：")
	if err != nil {
		return err
	}
	if !filepath.IsLocal(name) || name == "." || filepath.Base(name) != name ||
		strings.ContainsAny(name, "/\\") || strings.ContainsFunc(name, unicode.IsControl) {
		return errors.New("插件名称须是单个目录名，请填写名称并避开路径分隔符和控制字符")
	}
	id, err := read("插件标识（发布请使用自己的反向域名，例如 de.jianwang.demo 或 io.github.<user>.demo；本地示例 com.example.hello）：")
	if err != nil {
		return err
	}
	if err := os.Mkdir(name, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("init: %q 已存在，请使用其他插件名称", name)
		}
		return fmt.Errorf("创建插件目录 %q：%w", name, err)
	}
	kind := projectTypes[index-1]
	if err := createProject(name, id, name, kind); err != nil {
		_ = os.Remove(name)
		return err
	}
	quotedName := strings.ReplaceAll(name, "'", "'\\''")
	if kind.native {
		fmt.Fprintf(out, "项目已创建。进入目录后运行 make dev 或 make pkg：\n  cd './%s'\n  make dev\n", quotedName)
	} else {
		fmt.Fprintf(out, "项目已创建。进入目录后运行 dev 或 pkg：\n  cd './%s'\n  ext dev\n", quotedName)
	}
	return nil
}

func nativePlatform() (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	switch platform {
	case "linux-amd64", "linux-arm64":
		return platform, nil
	default:
		return "", fmt.Errorf("原生模板仅支持 linux-amd64 和 linux-arm64，当前平台为 %s", platform)
	}
}

func createProject(directory, id, name string, kind projectType) error {
	metadata := manifest.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		ID:            id,
		Name:          name,
		Version:       "0.1.0",
		License:       "MIT",
		HostAPI:       manifest.HostAPI{Min: manifest.HostAPIVersion, Max: manifest.HostAPIVersion},
	}
	if kind.web {
		metadata.Entry.Web = "web/index.html"
	}
	if kind.lua {
		metadata.Entry.Lua = "lua/init.lua"
	}
	platform := ""
	if kind.native {
		var err error
		platform, err = nativePlatform()
		if err != nil {
			return err
		}
		metadata.Entry.Native = "native/plugin"
		metadata.MinHostVersion = manifest.Version
		metadata.Native = &manifest.Native{
			Platform: platform,
			Exports:  []string{"v1.ping"},
			WebAPI:   kind.web,
		}
	}
	if err := metadata.Validate(); err != nil {
		return err
	}
	manifestJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	templateFiles := map[string]string{"README.md": "README.md"}
	if kind.web {
		templateFiles["web/index.html"] = "index.html"
		templateFiles["web/style.css"] = "style.css"
		if kind.lua || kind.native {
			templateFiles["web/app.js"] = "app.js"
		}
	}
	if kind.lua {
		templateFiles["lua/init.lua"] = "init.lua"
	}
	if kind.native {
		templateFiles["backend/main.go"] = "native.go.tmpl"
		templateFiles["go.mod"] = "go.mod.tmpl"
		templateFiles["Makefile"] = "Makefile.tmpl"
	}
	gitignore := "/.ext/\n/dist/\n"
	if kind.native {
		gitignore += "/native/plugin\n"
	}
	files := map[string][]byte{
		"manifest.json": append(manifestJSON, '\n'),
		".gitignore":    []byte(gitignore),
	}
	details := struct {
		ID       string
		Name     string
		Web      bool
		Lua      bool
		Native   bool
		Platform string
	}{id, name, kind.web, kind.lua, kind.native, platform}
	for path, source := range templateFiles {
		data, err := projectTemplates.ReadFile("templates/" + source)
		if err != nil {
			return err
		}
		if source != "README.md" && source != "index.html" && source != "native.go.tmpl" {
			files[path] = data
			continue
		}
		rendered, err := renderProjectTemplate(source, data, details)
		if err != nil {
			return err
		}
		files[path] = rendered
	}
	return writeProject(directory, files)
}

func renderProjectTemplate(name string, data []byte, details any) ([]byte, error) {
	var rendered strings.Builder
	if name == "native.go.tmpl" || name == "README.md" {
		tmpl, err := texttemplate.New(name).Parse(string(data))
		if err != nil {
			return nil, err
		}
		if err := tmpl.Execute(&rendered, details); err != nil {
			return nil, err
		}
		return []byte(rendered.String()), nil
	}
	tmpl, err := htmltemplate.New(name).Parse(string(data))
	if err != nil {
		return nil, err
	}
	if err := tmpl.Execute(&rendered, details); err != nil {
		return nil, err
	}
	return []byte(rendered.String()), nil
}

func writeProject(directory string, files map[string][]byte) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	paths := slices.Sorted(maps.Keys(files))
	for _, name := range paths {
		if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
			if err != nil {
				return err
			}
			return fmt.Errorf("init: %s already exists", name)
		}
		parent := filepath.Dir(name)
		if info, err := root.Lstat(parent); err == nil && !info.IsDir() {
			return fmt.Errorf("init: %s must be a directory", parent)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	var created []string
	complete := false
	defer func() {
		if !complete {
			for index := len(created) - 1; index >= 0; index-- {
				_ = root.Remove(created[index])
			}
		}
	}()
	for _, name := range paths {
		parent := filepath.Dir(name)
		if parent != "." {
			if err := root.Mkdir(parent, 0o755); err == nil {
				created = append(created, parent)
			} else if !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		created = append(created, name)
		_, writeErr := file.Write(files[name])
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return err
		}
	}
	complete = true
	return nil
}

func packageProject(args []string) error {
	positional, err := parseFlags(args, nil)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("pkg uses the current project directory")
	}
	pkg, err := bundle.LoadProject(".")
	if err != nil {
		return err
	}
	target, err := filepath.Abs(filepath.Join("dist", pkg.Filename()))
	if err != nil {
		return err
	}
	return writePackage(pkg, target)
}
