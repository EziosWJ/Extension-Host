package lua

import (
	"encoding/json"
	"testing"

	glua "github.com/yuin/gopher-lua"
)

func TestEmptyTableEncoding(t *testing.T) {
	L := glua.NewState()
	defer L.Close()

	tests := []struct {
		name  string
		value glua.LValue
		want  string
	}{
		{name: "empty slice", value: toLua(L, []any{}), want: "[]"},
		{name: "empty string slice", value: toLua(L, []string{}), want: "[]"},
		{name: "empty array table", value: newArrayTable(L), want: "[]"},
		{name: "non-empty array table", value: toLua(L, []any{"start"}), want: `["start"]`},
		{name: "empty object", value: toLua(L, map[string]any{}), want: "{}"},
		{name: "bare table", value: L.NewTable(), want: "{}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			converted, err := toGo(tc.value)
			if err != nil {
				t.Fatalf("toGo: %v", err)
			}
			data, err := json.Marshal(converted)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(data) != tc.want {
				t.Fatalf("encoded = %s, want %s", data, tc.want)
			}
		})
	}
}

func TestNestedEmptyArrayEncodesAsArray(t *testing.T) {
	L := glua.NewState()
	defer L.Close()

	service := L.NewTable()
	service.RawSetString("name", glua.LString("cryptdisks.service"))
	service.RawSetString("actions", newArrayTable(L))

	converted, err := toGo(service)
	if err != nil {
		t.Fatalf("toGo: %v", err)
	}
	data, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"actions":[],"name":"cryptdisks.service"}`
	if string(data) != want {
		t.Fatalf("encoded = %s, want %s", data, want)
	}
}
