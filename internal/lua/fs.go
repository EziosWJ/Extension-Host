package lua

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
)

func (r *Runtime) fsModule(L *glua.LState) *glua.LTable {
	module := L.NewTable()
	module.RawSetString("stat", L.NewFunction(fsStat))
	module.RawSetString("listDir", L.NewFunction(fsListDir))
	module.RawSetString("mkdirAll", L.NewFunction(fsMkdirAll))
	module.RawSetString("removeAll", L.NewFunction(fsRemoveAll))
	module.RawSetString("writeFile", L.NewFunction(fsWriteFile))
	return module
}

func fsStat(L *glua.LState) int {
	path := checkFSPath(L, 1)

	linfo, err := os.Lstat(path)
	if err != nil {
		return pushFSError(L, err)
	}
	isSymlink := linfo.Mode()&os.ModeSymlink != 0

	info, err := os.Stat(path)
	if err != nil {
		return pushFSError(L, err)
	}

	table := L.NewTable()
	table.RawSetString("name", glua.LString(info.Name()))
	table.RawSetString("size", glua.LNumber(info.Size()))
	table.RawSetString("mode", glua.LString(fmt.Sprintf("%04o", info.Mode().Perm())))
	table.RawSetString("modTime", glua.LString(info.ModTime().UTC().Format(time.RFC3339)))
	table.RawSetString("isDir", glua.LBool(info.IsDir()))
	table.RawSetString("isFile", glua.LBool(info.Mode().IsRegular()))
	table.RawSetString("isSymlink", glua.LBool(isSymlink))
	L.Push(table)
	return 1
}

func fsListDir(L *glua.LState) int {
	path := checkFSPath(L, 1)

	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return pushFSError(L, err)
	}

	entries := newArrayTable(L)
	for _, entry := range dirEntries {
		entryType := entry.Type()
		item := L.NewTable()
		item.RawSetString("name", glua.LString(entry.Name()))
		item.RawSetString("isDir", glua.LBool(entry.IsDir()))
		item.RawSetString("isFile", glua.LBool(entryType.IsRegular()))
		item.RawSetString("isSymlink", glua.LBool(entryType&os.ModeSymlink != 0))
		if info, infoErr := entry.Info(); infoErr == nil {
			item.RawSetString("size", glua.LNumber(info.Size()))
			item.RawSetString("modTime", glua.LString(info.ModTime().UTC().Format(time.RFC3339)))
		}
		entries.Append(item)
	}
	L.Push(entries)
	return 1
}

func fsMkdirAll(L *glua.LState) int {
	path := checkFSPath(L, 1)
	perm := os.FileMode(0o755)
	if value := L.Get(2); value != glua.LNil {
		perm = parseFileMode(L, 2, value, "mode")
	}

	if err := os.MkdirAll(path, perm); err != nil {
		return pushFSError(L, err)
	}
	L.Push(glua.LTrue)
	return 1
}

func fsRemoveAll(L *glua.LState) int {
	path := checkFSPath(L, 1)
	cleaned := filepath.Clean(path)
	if cleaned == "/" || cleaned == string(filepath.Separator) || cleaned == "." || cleaned == ".." {
		L.ArgError(1, "refusing to remove root or relative dot path")
		return 0
	}

	if err := os.RemoveAll(path); err != nil {
		return pushFSError(L, err)
	}
	L.Push(glua.LTrue)
	return 1
}

func fsWriteFile(L *glua.LState) int {
	path := checkFSPath(L, 1)
	L.CheckType(2, glua.LTString)
	data := L.CheckString(2)

	perm := os.FileMode(0o644)
	atomic := true
	mkdir := false
	if options := L.OptTable(3, nil); options != nil {
		if value := options.RawGetString("mode"); value != glua.LNil {
			perm = parseFileMode(L, 3, value, "mode")
		}
		if value := options.RawGetString("atomic"); value != glua.LNil {
			flag, ok := value.(glua.LBool)
			if !ok {
				L.ArgError(3, "atomic must be a boolean")
				return 0
			}
			atomic = bool(flag)
		}
		if value := options.RawGetString("mkdir"); value != glua.LNil {
			flag, ok := value.(glua.LBool)
			if !ok {
				L.ArgError(3, "mkdir must be a boolean")
				return 0
			}
			mkdir = bool(flag)
		}
	}

	if mkdir {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return pushFSError(L, err)
		}
	}

	var err error
	if atomic {
		err = writeFileAtomic(path, []byte(data), perm)
	} else {
		err = writeFileDirect(path, []byte(data), perm)
	}
	if err != nil {
		return pushFSError(L, err)
	}
	L.Push(glua.LTrue)
	return 1
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".host-fs-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func writeFileDirect(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func checkFSPath(L *glua.LState, arg int) string {
	L.CheckType(arg, glua.LTString)
	path := L.CheckString(arg)
	if path == "" {
		L.ArgError(arg, "path must be a non-empty string")
		return ""
	}
	if strings.ContainsRune(path, 0) {
		L.ArgError(arg, "path must not contain NUL bytes")
		return ""
	}
	return path
}

func parseFileMode(L *glua.LState, arg int, value glua.LValue, field string) os.FileMode {
	switch typed := value.(type) {
	case glua.LNumber:
		number := float64(typed)
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < 0 || number > 0o777 {
			L.ArgError(arg, fmt.Sprintf("%s must be an integer or octal string between 0 and 0777", field))
			return 0
		}
		return os.FileMode(uint32(number))
	case glua.LString:
		parsed, err := strconv.ParseUint(string(typed), 8, 32)
		if err != nil || parsed > 0o777 {
			L.ArgError(arg, fmt.Sprintf("%s must be an integer or octal string between 0 and 0777", field))
			return 0
		}
		return os.FileMode(parsed)
	default:
		L.ArgError(arg, fmt.Sprintf("%s must be an integer or octal string between 0 and 0777", field))
		return 0
	}
}

func pushFSError(L *glua.LState, err error) int {
	L.Push(glua.LNil)
	L.Push(glua.LString(err.Error()))
	return 2
}
