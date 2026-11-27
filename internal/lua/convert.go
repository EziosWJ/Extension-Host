package lua

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	glua "github.com/yuin/gopher-lua"
)

const maxConvertDepth = 64

// arrayMarkerKey marks a Lua table as a JSON array on its metatable. Lua has no
// distinct array type, so an empty table would otherwise encode as an object.
const arrayMarkerKey = "__host_array"

// newArrayTable returns an empty Lua table that converts to a JSON array even
// when no elements are appended.
func newArrayTable(L *glua.LState) *glua.LTable {
	table := L.NewTable()
	metatable := L.NewTable()
	metatable.RawSetString(arrayMarkerKey, glua.LTrue)
	L.SetMetatable(table, metatable)
	return table
}

func jsonArray(L *glua.LState) int {
	array := newArrayTable(L)
	if arg := L.Get(1); arg != glua.LNil {
		source, ok := arg.(*glua.LTable)
		if !ok {
			L.ArgError(1, "expected a table")
			return 0
		}
		count := 0
		source.ForEach(func(_, _ glua.LValue) { count++ })
		// If all count positions are present, there can be no holes or extra keys.
		for index := 1; index <= count; index++ {
			value := source.RawGet(glua.LNumber(index))
			if value == glua.LNil {
				L.ArgError(1, "expected a table with consecutive integer keys starting at 1")
				return 0
			}
			array.RawSetInt(index, value)
		}
	}
	L.Push(array)
	return 1
}

func isArrayTable(table *glua.LTable) bool {
	metatable, ok := table.Metatable.(*glua.LTable)
	if !ok {
		return false
	}
	return glua.LVAsBool(metatable.RawGetString(arrayMarkerKey))
}

// toGo converts a Lua value to plain JSON-compatible Go values.
func toGo(value glua.LValue) (any, error) {
	return toGoDepth(value, 0, make(map[*glua.LTable]bool))
}

func toGoDepth(value glua.LValue, depth int, seen map[*glua.LTable]bool) (any, error) {
	if depth > maxConvertDepth {
		return nil, fmt.Errorf("lua value is nested too deeply")
	}
	switch typed := value.(type) {
	case *glua.LNilType:
		return nil, nil
	case glua.LBool:
		return bool(typed), nil
	case glua.LNumber:
		return float64(typed), nil
	case glua.LString:
		return string(typed), nil
	case *glua.LTable:
		return tableToGo(typed, depth, seen)
	default:
		return nil, fmt.Errorf("lua value of type %s cannot be converted to JSON", value.Type())
	}
}

func tableToGo(table *glua.LTable, depth int, seen map[*glua.LTable]bool) (any, error) {
	if seen[table] {
		return nil, fmt.Errorf("lua table contains a cycle")
	}
	seen[table] = true
	defer delete(seen, table)

	type entry struct {
		key   glua.LValue
		value glua.LValue
	}
	var entries []entry
	allNumbers := true
	allStrings := true
	count := 0
	maxIndex := 0
	table.ForEach(func(key, value glua.LValue) {
		entries = append(entries, entry{key: key, value: value})
		count++
		switch typed := key.(type) {
		case glua.LNumber:
			allStrings = false
			index := int(typed)
			if float64(index) != float64(typed) || index < 1 {
				allNumbers = false
				return
			}
			if index > maxIndex {
				maxIndex = index
			}
		case glua.LString:
			allNumbers = false
		default:
			allNumbers = false
			allStrings = false
		}
	})

	switch {
	case count == 0:
		if isArrayTable(table) {
			return []any{}, nil
		}
		return map[string]any{}, nil
	case allNumbers && maxIndex == count:
		array := make([]any, maxIndex)
		for _, item := range entries {
			index := int(item.key.(glua.LNumber))
			value, err := toGoDepth(item.value, depth+1, seen)
			if err != nil {
				return nil, err
			}
			array[index-1] = value
		}
		return array, nil
	case allStrings:
		object := make(map[string]any, count)
		for _, item := range entries {
			value, err := toGoDepth(item.value, depth+1, seen)
			if err != nil {
				return nil, err
			}
			object[string(item.key.(glua.LString))] = value
		}
		return object, nil
	default:
		return nil, fmt.Errorf("lua table must use either consecutive integer keys or string keys")
	}
}

// toLua converts a plain Go value to a Lua value.
func toLua(L *glua.LState, value any) glua.LValue {
	switch typed := value.(type) {
	case nil:
		return glua.LNil
	case bool:
		return glua.LBool(typed)
	case string:
		return glua.LString(typed)
	case []byte:
		return glua.LString(string(typed))
	case float64:
		return glua.LNumber(typed)
	case float32:
		return glua.LNumber(float64(typed))
	case int:
		return glua.LNumber(float64(typed))
	case int64:
		return glua.LNumber(float64(typed))
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return glua.LString(typed.String())
		}
		return glua.LNumber(number)
	case time.Time:
		return glua.LString(typed.Format(time.RFC3339Nano))
	case []any:
		table := newArrayTable(L)
		for _, item := range typed {
			table.Append(toLua(L, item))
		}
		return table
	case []string:
		table := newArrayTable(L)
		for _, item := range typed {
			table.Append(glua.LString(item))
		}
		return table
	case map[string]any:
		table := L.NewTable()
		for key, item := range typed {
			table.RawSetString(key, toLua(L, item))
		}
		return table
	case map[string]string:
		table := L.NewTable()
		for key, item := range typed {
			table.RawSetString(key, glua.LString(item))
		}
		return table
	default:
		return glua.LString(fmt.Sprint(value))
	}
}

// storageValue converts a Lua scalar to a string stored in the business database.
func storageValue(value glua.LValue) (string, error) {
	switch typed := value.(type) {
	case glua.LString:
		return string(typed), nil
	case glua.LNumber:
		return strconv.FormatFloat(float64(typed), 'g', -1, 64), nil
	case glua.LBool:
		return strconv.FormatBool(bool(typed)), nil
	default:
		return "", fmt.Errorf("storage value must be a string, number or boolean, got %s", value.Type())
	}
}

// sqlArgs converts a Lua array to SQL positional arguments.
func sqlArgs(table *glua.LTable) ([]any, error) {
	if table == nil {
		return nil, nil
	}
	values := make(map[int]glua.LValue)
	maxIndex := 0
	var conversionErr error
	table.ForEach(func(key, value glua.LValue) {
		if conversionErr != nil {
			return
		}
		number, ok := key.(glua.LNumber)
		if !ok {
			conversionErr = fmt.Errorf("query arguments must be an array")
			return
		}
		index := int(number)
		if index < 1 || float64(index) != float64(number) {
			conversionErr = fmt.Errorf("query arguments must be an array")
			return
		}
		if index > maxIndex {
			maxIndex = index
		}
		values[index] = value
	})
	if conversionErr != nil {
		return nil, conversionErr
	}
	if len(values) != maxIndex {
		return nil, fmt.Errorf("query arguments must be a consecutive array")
	}
	args := make([]any, maxIndex)
	for index, value := range values {
		switch typed := value.(type) {
		case *glua.LNilType:
			args[index-1] = nil
		case glua.LBool:
			args[index-1] = bool(typed)
		case glua.LNumber:
			args[index-1] = float64(typed)
		case glua.LString:
			args[index-1] = string(typed)
		default:
			return nil, fmt.Errorf("query argument %d has unsupported type %s", index, value.Type())
		}
	}
	return args, nil
}
