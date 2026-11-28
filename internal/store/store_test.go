package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestHostRecordsPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "host.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	installed := time.Now().UTC().Truncate(time.Millisecond)
	record := Plugin{
		ID:          "com.example.test",
		Name:        "Test",
		Version:     "1.0.0",
		Manifest:    []byte(`{"schemaVersion":1}`),
		Config:      map[string]any{"greeting": "hi"},
		Enabled:     false,
		InstalledAt: installed,
	}
	if err := database.InsertPlugin(ctx, record); err != nil {
		t.Fatalf("InsertPlugin: %v", err)
	}
	if err := database.SetEnabled(ctx, record.ID, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := database.SetConfig(ctx, record.ID, map[string]any{"greeting": "hello"}, nil); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	database, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer database.Close()
	plugins, err := database.ListPlugins(ctx)
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins = %d", len(plugins))
	}
	loaded := plugins[0]
	if !loaded.Enabled {
		t.Fatal("enabled intent was lost")
	}
	if loaded.Config["greeting"] != "hello" {
		t.Fatalf("config = %#v", loaded.Config)
	}
	if !loaded.InstalledAt.Equal(installed) {
		t.Fatalf("installedAt = %v, want %v", loaded.InstalledAt, installed)
	}
	if err := database.DeletePlugin(ctx, record.ID); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}
	plugins, err = database.ListPlugins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins after delete = %d", len(plugins))
	}
}

func TestBusinessDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "business.db")
	business, err := OpenBusiness(ctx, path)
	if err != nil {
		t.Fatalf("OpenBusiness: %v", err)
	}
	if err := business.Set(ctx, "visits", "1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value, ok, err := business.Get(ctx, "visits")
	if err != nil || !ok || value != "1" {
		t.Fatalf("Get = %q, %v, %v", value, ok, err)
	}
	if _, err := business.Exec(ctx, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`, nil); err != nil {
		t.Fatalf("Exec create: %v", err)
	}
	affected, err := business.Exec(ctx, `INSERT INTO notes (body) VALUES (?)`, []any{"hello"})
	if err != nil || affected != 1 {
		t.Fatalf("Exec insert = %d, %v", affected, err)
	}
	result, err := business.Query(ctx, `SELECT id, body FROM notes`, nil)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0][1] != "hello" {
		t.Fatalf("rows = %#v", result.Rows)
	}
	if err := business.Delete(ctx, "visits"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := business.Get(ctx, "visits"); err != nil || ok {
		t.Fatalf("Get after delete = %v, %v", ok, err)
	}
	if err := business.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	business, err = OpenBusiness(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer business.Close()
	result, err = business.Query(ctx, `SELECT body FROM notes`, nil)
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("reopened rows = %#v, %v", result.Rows, err)
	}
}
