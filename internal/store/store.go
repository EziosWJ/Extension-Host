// Package store persists host metadata and per-plugin business data in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

// Plugin is the persisted installation record.
type Plugin struct {
	ID               string
	Name             string
	Version          string
	Manifest         []byte
	Config           map[string]any
	Enabled          bool
	InstalledAt      time.Time
	PackageDirectory string
}

// Store owns host.db.
type Store struct {
	db *sql.DB
}

// Open opens or creates the host database.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := openDatabase(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open host database: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS plugins (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	version TEXT NOT NULL,
	manifest TEXT NOT NULL,
	config TEXT NOT NULL DEFAULT '{}',
	enabled INTEGER NOT NULL DEFAULT 0,
	installed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS development_packages (
	plugin_id TEXT PRIMARY KEY,
	directory TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS host_settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize host database: %w", err)
	}
	return nil
}

// Close closes the host database.
func (s *Store) Close() error {
	return s.db.Close()
}

// ListPlugins returns all installed plugins ordered by name.
func (s *Store) ListPlugins(ctx context.Context) ([]Plugin, error) {
	rows, err := s.db.QueryContext(ctx, pluginQuery+` ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list plugins: %w", err)
	}
	defer rows.Close()

	var plugins []Plugin
	for rows.Next() {
		plugin, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list plugins: %w", err)
	}
	return plugins, nil
}

// GetPlugin returns one plugin.
func (s *Store) GetPlugin(ctx context.Context, id string) (Plugin, bool, error) {
	row := s.db.QueryRowContext(ctx, pluginQuery+` WHERE id = ?`, id)
	plugin, err := scanPlugin(row)
	if err == sql.ErrNoRows {
		return Plugin{}, false, nil
	}
	if err != nil {
		return Plugin{}, false, err
	}
	return plugin, true, nil
}

const pluginQuery = `SELECT id, name, version, manifest, config, enabled, installed_at,
	COALESCE(directory, '') FROM plugins LEFT JOIN development_packages ON plugin_id = id`

// SetPackage publishes a package directory and its metadata in the same transaction.
func (s *Store) SetPackage(ctx context.Context, plugin Plugin) error {
	return s.setPackage(ctx, plugin, false)
}

// SetDevelopmentPackage also updates the development plugin's config and enable intent.
func (s *Store) SetDevelopmentPackage(ctx context.Context, plugin Plugin) error {
	return s.setPackage(ctx, plugin, true)
}

func (s *Store) setPackage(ctx context.Context, plugin Plugin, development bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		`UPDATE plugins SET name = ?, version = ?, manifest = ? WHERE id = ?`,
		plugin.Name, plugin.Version, string(plugin.Manifest), plugin.ID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("update plugin %s: not installed", plugin.ID)
	}
	if development {
		config, err := json.Marshal(plugin.Config)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugins SET config = ?, enabled = ? WHERE id = ?`,
			string(config), plugin.Enabled, plugin.ID); err != nil {
			return err
		}
	}
	// Retain the legacy table name so existing development snapshots still resolve.
	_, err = tx.ExecContext(ctx, `INSERT INTO development_packages (plugin_id, directory) VALUES (?, ?)
		ON CONFLICT(plugin_id) DO UPDATE SET directory = excluded.directory`, plugin.ID, plugin.PackageDirectory)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// HomePlugin returns the saved standalone home plugin ID. An empty ID means the management page.
func (s *Store) HomePlugin(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM host_settings WHERE key = 'home_plugin'`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// SetHomePlugin saves the standalone home plugin.
func (s *Store) SetHomePlugin(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO host_settings (key, value) VALUES ('home_plugin', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, id)
	return err
}

// InsertPlugin records a new installation.
func (s *Store) InsertPlugin(ctx context.Context, plugin Plugin) error {
	config, err := json.Marshal(plugin.Config)
	if err != nil {
		return fmt.Errorf("encode plugin config: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO plugins (id, name, version, manifest, config, enabled, installed_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		plugin.ID, plugin.Name, plugin.Version, string(plugin.Manifest), string(config), plugin.Enabled,
		plugin.InstalledAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert plugin: %w", err)
	}
	return nil
}

// SetEnabled persists the enable intent.
func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE plugins SET enabled = ? WHERE id = ?`, enabled, id)
	if err != nil {
		return fmt.Errorf("update plugin: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update plugin: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("update plugin %s: not installed", id)
	}
	return nil
}

// SetConfig persists a plugin configuration.
func (s *Store) SetConfig(ctx context.Context, id string, config map[string]any) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode plugin config: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE plugins SET config = ? WHERE id = ?`, string(data), id)
	if err != nil {
		return fmt.Errorf("update plugin config: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update plugin config: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("update plugin %s: not installed", id)
	}
	return nil
}

// DeletePlugin removes an installation record.
func (s *Store) DeletePlugin(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM development_packages WHERE plugin_id = ?`, id); err != nil {
		return fmt.Errorf("delete development package: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete plugin: %w", err)
	}
	return tx.Commit()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPlugin(row scanner) (Plugin, error) {
	var (
		plugin      Plugin
		manifestRaw string
		configRaw   string
		installedAt string
		enabled     int
	)
	if err := row.Scan(&plugin.ID, &plugin.Name, &plugin.Version, &manifestRaw, &configRaw, &enabled, &installedAt, &plugin.PackageDirectory); err != nil {
		if err == sql.ErrNoRows {
			return Plugin{}, err
		}
		return Plugin{}, fmt.Errorf("read plugin: %w", err)
	}
	plugin.Manifest = []byte(manifestRaw)
	if err := json.Unmarshal([]byte(configRaw), &plugin.Config); err != nil {
		return Plugin{}, fmt.Errorf("decode config for %s: %w", plugin.ID, err)
	}
	if plugin.Config == nil {
		plugin.Config = map[string]any{}
	}
	plugin.Enabled = enabled != 0
	parsed, err := time.Parse(time.RFC3339Nano, installedAt)
	if err != nil {
		return Plugin{}, fmt.Errorf("decode install time for %s: %w", plugin.ID, err)
	}
	plugin.InstalledAt = parsed
	return plugin, nil
}

// Business is the SQLite database owned by one plugin.
type Business struct {
	db *sql.DB
}

// OpenBusiness opens or creates a plugin business database.
func OpenBusiness(ctx context.Context, path string) (*Business, error) {
	db, err := openDatabase(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open plugin database: %w", err)
	}
	const schema = `CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize plugin database: %w", err)
	}
	return &Business{db: db}, nil
}

// Close closes the business database.
func (b *Business) Close() error {
	return b.db.Close()
}

// Get reads a key-value entry.
func (b *Business) Get(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := b.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read plugin storage: %w", err)
	}
	return value, true, nil
}

// Set writes a key-value entry.
func (b *Business) Set(ctx context.Context, key, value string) error {
	_, err := b.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write plugin storage: %w", err)
	}
	return nil
}

// Delete removes a key-value entry.
func (b *Business) Delete(ctx context.Context, key string) error {
	if _, err := b.db.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete plugin storage: %w", err)
	}
	return nil
}

// Result is a generic query result.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Query runs a read-only or read-write SQL query.
func (b *Business) Query(ctx context.Context, query string, args []any) (*Result, error) {
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("plugin query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("plugin query: %w", err)
	}
	result := &Result{Columns: columns}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("plugin query: %w", err)
		}
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plugin query: %w", err)
	}
	return result, nil
}

// Exec runs a statement and returns the affected row count.
func (b *Business) Exec(ctx context.Context, statement string, args []any) (int64, error) {
	result, err := b.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return 0, fmt.Errorf("plugin exec: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("plugin exec: %w", err)
	}
	return affected, nil
}

func openDatabase(ctx context.Context, path string) (*sql.DB, error) {
	// Use a file URI so characters such as '?' in the data directory stay part of the path.
	dsn := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)",
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
