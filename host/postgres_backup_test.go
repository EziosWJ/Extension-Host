//go:build linux

package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EziosWJ/Extension-Host/host"
	"github.com/EziosWJ/Extension-Host/internal/bundle"
	"github.com/EziosWJ/Extension-Host/internal/store"
)

func installPostgresBackup(t *testing.T, ctx context.Context, extensionHost *host.Host) host.Plugin {
	t.Helper()
	pkg, err := bundle.LoadProject(filepath.Join("..", "examples", "postgres-backup", "plugin"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(pkg.Files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest["permissions"]; exists {
		t.Fatal("the PostgreSQL backup manifest must omit permissions")
	}
	archive, err := pkg.Zip()
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := extensionHost.Install(ctx, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return plugin
}

func postgresBackupRequest(t *testing.T, extensionHost *host.Host) func(string, string, any, int, any) {
	t.Helper()
	handler := extensionHost.Handler(host.HTTPOptions{})
	return func(method, path string, payload any, status int, result any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		exchange := httptest.NewRecorder()
		handler.ServeHTTP(exchange, httptest.NewRequest(method, path, bytes.NewReader(body)))
		if exchange.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, exchange.Code, status, exchange.Body)
		}
		if result != nil {
			if err := json.Unmarshal(exchange.Body.Bytes(), result); err != nil {
				t.Fatalf("decode %s: %v (%s)", path, err, exchange.Body)
			}
		}
	}
}

func TestPostgresBackupDirectory(t *testing.T) {
	ctx := testContext(t)
	extensionHost, dataDir := newTestHost(t, ctx)
	plugin := installPostgresBackup(t, ctx, extensionHost)
	request := postgresBackupRequest(t, extensionHost)
	configPath := "/api/v1/extensions/" + plugin.ID + "/config"
	var saved host.Plugin
	dir := filepath.Join(dataDir, "backup archives", "nested")
	request(http.MethodPut, configPath, map[string]any{"备份目录": dir, "并行任务上限": 3}, http.StatusOK, &saved)
	if saved.Status != host.StatusStopped || saved.Config["备份目录"] != dir || saved.Config["并行任务上限"] != float64(3) {
		t.Fatalf("settings saved while stopped = %+v", saved)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("created directory contents = %v, err = %v", files, err)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var problem struct{ Error string }
	request(http.MethodPut, configPath, map[string]any{"备份目录": filepath.Join(blocker, "invalid"), "并行任务上限": 1}, http.StatusBadRequest, &problem)
	if !strings.Contains(problem.Error, "目录无法使用") {
		t.Fatalf("unexpected unusable directory message: %q", problem.Error)
	}
	request(http.MethodPut, configPath, map[string]any{"备份目录": "relative"}, http.StatusBadRequest, &problem)
	for _, invalid := range []float64{0, 1.5, 17} {
		request(http.MethodPut, configPath, map[string]any{"并行任务上限": invalid}, http.StatusBadRequest, &problem)
	}
	var persisted host.Plugin
	request(http.MethodGet, "/api/v1/extensions/"+plugin.ID, nil, http.StatusOK, &persisted)
	if persisted.Config["备份目录"] != dir || persisted.Config["并行任务上限"] != float64(3) {
		t.Fatalf("settings after failed save = %+v", persisted.Config)
	}

	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	var sources struct {
		Sources []json.RawMessage
	}
	request(http.MethodGet, plugin.APIPath+"sources", nil, http.StatusOK, &sources)
	if sources.Sources == nil || len(sources.Sources) != 0 {
		t.Fatalf("fresh plugin sources = %v, want an empty JSON array", sources.Sources)
	}

	var cleared host.Plugin
	request(http.MethodPut, configPath, map[string]any{"备份目录": nil}, http.StatusOK, &cleared)
	if cleared.Config["备份目录"] != nil || cleared.Config["并行任务上限"] != float64(3) {
		t.Fatalf("cleared directory config = %+v", cleared.Config)
	}
}

type postgresBackupJob struct {
	ID          string
	Status      string
	Error       string
	Trigger     string
	ArchivePath string `json:"archive_path"`
	Previous    string
	Displaced   string
	Cleaned     int `json:"cleaned_displaced"`
}

func TestPostgresBackupWorkflow(t *testing.T) {
	ctx := testContext(t)
	toolsDir := t.TempDir()
	stub, err := os.ReadFile("testdata/postgres-backup/pg-client.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"psql", "pg_dump", "pg_restore"} {
		if err := os.WriteFile(filepath.Join(toolsDir, name), stub, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	catalog := t.TempDir()
	if err := os.WriteFile(filepath.Join(catalog, "original"), []byte("10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PG_BACKUP_TEST_CATALOG", catalog)
	t.Setenv("PGSERVICE", "inherited-test-service")
	t.Setenv("PGHOSTADDR", "192.0.2.1")

	extensionHost, dataDir := newTestHost(t, ctx)
	plugin := installPostgresBackup(t, ctx, extensionHost)
	request := postgresBackupRequest(t, extensionHost)
	var config host.Plugin
	archives := filepath.Join(dataDir, "test archives")
	request(http.MethodPut, "/api/v1/extensions/"+plugin.ID+"/config",
		map[string]any{"备份目录": archives}, http.StatusOK, &config)
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	call := func(method, route string, payload any, status int, result any) {
		t.Helper()
		request(method, plugin.APIPath+route, payload, status, result)
	}
	operation := func(method, route string, payload, result any) {
		t.Helper()
		var started struct{ Operation string }
		call(method, route, payload, http.StatusAccepted, &started)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			var outcome struct {
				Status string
				Error  string
				Data   json.RawMessage
			}
			call(http.MethodGet, "operation?id="+started.Operation, nil, http.StatusOK, &outcome)
			switch outcome.Status {
			case "succeeded":
				if err := json.Unmarshal(outcome.Data, result); err != nil {
					t.Fatal(err)
				}
				return
			case "failed":
				t.Fatalf("%s: %s", route, outcome.Error)
			}
		}
		t.Fatalf("%s did not complete", route)
	}
	waitJob := func(route, id, trigger, status string) postgresBackupJob {
		t.Helper()
		var last postgresBackupJob
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			var records struct{ Jobs []postgresBackupJob }
			call(http.MethodGet, route, nil, http.StatusOK, &records)
			for _, job := range records.Jobs {
				if (id == "" || job.ID == id) && (trigger == "" || job.Trigger == trigger) {
					if job.Status == status {
						return job
					}
					last = job
				}
			}
		}
		t.Fatalf("%s: job %q never reached %s, last record: %+v", route, id, status, last)
		return postgresBackupJob{}
	}

	var source struct {
		ID            string
		PasswordSaved bool
		Password      *string
	}
	operation(http.MethodPut, "sources", map[string]any{
		"name": "Fixture", "host": "fixture.invalid", "port": 5432, "username": "fixture", "database": "control", "password": "fixture-only",
	}, &source)
	if source.ID == "" || !source.PasswordSaved || source.Password != nil {
		t.Fatalf("unexpected public source: %+v", source)
	}
	var probe struct{ Rows []struct{ Name string } }
	operation(http.MethodPost, "probe", map[string]any{"source": source.ID}, &probe)
	if len(probe.Rows) != 1 || probe.Rows[0].Name != "original" {
		t.Fatalf("database list: %+v", probe.Rows)
	}
	operation(http.MethodPost, "probe", map[string]any{"source": source.ID, "database": "original"}, &probe)
	if len(probe.Rows) != 1 || probe.Rows[0].Name != "public" {
		t.Fatalf("schema list: %+v", probe.Rows)
	}

	target := map[string]any{"source": source.ID, "database": "original"}
	var started struct{ ID string }
	call(http.MethodPost, "backups", target, http.StatusAccepted, &started)
	backup := waitJob("backups", started.ID, "manual", "succeeded")
	if backup.ArchivePath != filepath.Join(archives, backup.ID+".dump") {
		t.Fatalf("archive path: %q", backup.ArchivePath)
	}
	info, err := os.Stat(backup.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode: %o", info.Mode().Perm())
	}

	var schedule struct{ ID string }
	call(http.MethodPut, "schedules", map[string]any{
		"name": "Fixture schedule", "source": source.ID, "database": "original", "expression": "@every 1s",
	}, http.StatusOK, &schedule)
	waitJob("backups", "", "scheduled", "succeeded")
	call(http.MethodDelete, "schedules", map[string]any{"id": schedule.ID}, http.StatusOK, nil)

	failure := filepath.Join(catalog, "fail-dump")
	if err := os.WriteFile(failure, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	call(http.MethodPost, "backups", target, http.StatusAccepted, &started)
	failed := waitJob("backups", started.ID, "manual", "failed")
	if failed.Error != "fixture dump failed" || failed.ArchivePath != "" {
		t.Fatalf("failed backup: %+v", failed)
	}
	for _, suffix := range []string{".dump", ".dump.partial"} {
		if _, err := os.Stat(filepath.Join(archives, failed.ID+suffix)); !os.IsNotExist(err) {
			t.Fatalf("failed backup left %s: %v", suffix, err)
		}
	}
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"new", "replace"} {
		database := "original"
		if mode == "new" {
			database = "published"
		}
		call(http.MethodPost, "restores", map[string]any{
			"backup": backup.ID, "source": source.ID, "database": database, "control": "control", "mode": mode, "portable": true,
		}, http.StatusAccepted, &started)
		assertOID := func(name, expected string) {
			t.Helper()
			oid, err := os.ReadFile(filepath.Join(catalog, name))
			if err != nil || strings.TrimSpace(string(oid)) != expected {
				t.Fatalf("catalog %q: OID %q, error %v, want %q", name, oid, err, expected)
			}
		}
		if mode == "new" {
			waitJob("restores", started.ID, "", "succeeded")
			assertOID(database, "20")
			continue
		}
		waitJob("restores", started.ID, "", "ready")
		assertOID(database, "10")
		action := func(action, confirm string, status int) {
			t.Helper()
			call(http.MethodPost, "restore-action", map[string]any{
				"id": started.ID, "action": action, "confirm": confirm,
			}, status, nil)
		}
		action("switch", "wrong", http.StatusBadRequest)
		if err := os.WriteFile(filepath.Join(catalog, database), []byte("99\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		action("switch", database, http.StatusAccepted)
		waitJob("restores", started.ID, "", "needs_review")
		assertOID(database, "99")
		action("check", "", http.StatusAccepted)
		waitJob("restores", started.ID, "", "needs_review")
		if err := os.WriteFile(filepath.Join(catalog, database), []byte("10\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		action("check", "", http.StatusAccepted)
		waitJob("restores", started.ID, "", "ready")
		action("switch", database, http.StatusAccepted)
		replaced := waitJob("restores", started.ID, "", "succeeded")
		assertOID(database, "21")
		assertOID(replaced.Previous, "10")
		action("rollback", database, http.StatusAccepted)
		rolledBack := waitJob("restores", started.ID, "", "rolled_back")
		assertOID(database, "10")
		assertOID(rolledBack.Displaced, "21")
		action("clean", database, http.StatusAccepted)
		if cleaned := waitJob("restores", started.ID, "", "rolled_back"); cleaned.Cleaned != 1 {
			t.Fatalf("cleaned record: %+v", cleaned)
		}
		if _, err := os.Stat(filepath.Join(catalog, rolledBack.Displaced)); !os.IsNotExist(err) {
			t.Fatalf("displaced database still exists: %v", err)
		}
	}
	call(http.MethodDelete, "backups", map[string]any{"id": backup.ID}, http.StatusOK, nil)
	if _, err := os.Stat(backup.ArchivePath); !os.IsNotExist(err) {
		t.Fatalf("deleted archive still exists: %v", err)
	}
}

func TestPostgresBackupLegacyStorage(t *testing.T) {
	ctx := testContext(t)
	extensionHost, dataDir := newTestHost(t, ctx)
	plugin := installPostgresBackup(t, ctx, extensionHost)
	pluginDir := filepath.Join(dataDir, "plugins", plugin.ID)
	archive := filepath.Join(pluginDir, "data", "old.dump")
	if err := os.MkdirAll(filepath.Dir(archive), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("old fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	business, err := store.OpenBusiness(ctx, filepath.Join(pluginDir, "business.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = business.Close() })
	if _, err := business.Exec(ctx, `CREATE TABLE backups (
		id TEXT PRIMARY KEY, target TEXT NOT NULL, status TEXT NOT NULL, started_at TEXT NOT NULL,
		file_name TEXT, error TEXT)`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := business.Exec(ctx, `INSERT INTO backups VALUES
		('old', 'legacy target', 'succeeded', '2026-01-01T00:00:00Z', 'old.dump', NULL),
		('interrupted', 'legacy target', 'running', '2026-01-01T00:00:00Z', NULL, NULL)`, nil); err != nil {
		t.Fatal(err)
	}
	if err := business.Set(ctx, "connection", `{"host":"fixture.invalid","port":5432,"database":"legacy","username":"fixture","password":"fixture-only"}`); err != nil {
		t.Fatal(err)
	}
	if err := business.Set(ctx, "schedule", "@every 24h"); err != nil {
		t.Fatal(err)
	}
	request := postgresBackupRequest(t, extensionHost)
	if err := extensionHost.Enable(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var sources struct {
			Sources []struct {
				Database      string
				PasswordSaved bool
			}
		}
		request(http.MethodGet, plugin.APIPath+"sources", nil, http.StatusOK, &sources)
		if len(sources.Sources) != 1 || sources.Sources[0].Database != "legacy" || !sources.Sources[0].PasswordSaved {
			t.Fatalf("migrated sources: %+v", sources.Sources)
		}
		var result struct{ Jobs []postgresBackupJob }
		request(http.MethodGet, plugin.APIPath+"backups", nil, http.StatusOK, &result)
		if len(result.Jobs) != 2 || result.Jobs[0].Status != "failed" || result.Jobs[1].ArchivePath != archive {
			t.Fatalf("migrated archive history: %+v", result.Jobs)
		}
		var schedules struct{ Schedules []struct{ Expression string } }
		request(http.MethodGet, plugin.APIPath+"schedules", nil, http.StatusOK, &schedules)
		if len(schedules.Schedules) != 1 || schedules.Schedules[0].Expression != "@every 24h" {
			t.Fatalf("migrated schedules: %+v", schedules.Schedules)
		}
		if err := extensionHost.Stop(ctx, plugin.ID); err != nil {
			t.Fatal(err)
		}
		if err := extensionHost.Start(ctx, plugin.ID); err != nil {
			t.Fatal(err)
		}
	}
}
