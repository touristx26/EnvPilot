// Package store 提供网关的 SQLite 持久化：仅存储环境元数据。
// 使用 modernc.org/sqlite（纯 Go，无 CGO，交叉编译友好）。
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store SQLite 持久化句柄（WAL 模式，支持并发读）。
type Store struct {
	db *sql.DB
}

// EnvRecord 环境表的行记录。Tags/Capabilities 为 JSON 编码字符串。
type EnvRecord struct {
	ID           string
	Name         string
	Type         string
	Tags         string // JSON: map[string]string
	OS           string
	Arch         string
	Capabilities string // JSON: []string
	RegisteredAt time.Time
	LastSeen     time.Time
}

// Open 打开（或初始化）位于 path 的 SQLite 数据库。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS environments (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL UNIQUE,
	type          TEXT NOT NULL,
	tags          TEXT NOT NULL DEFAULT '{}',
	os            TEXT NOT NULL DEFAULT '',
	arch          TEXT NOT NULL DEFAULT '',
	capabilities  TEXT NOT NULL DEFAULT '[]',
	registered_at TIMESTAMP NOT NULL,
	last_seen     TIMESTAMP NOT NULL,
	online        INTEGER NOT NULL DEFAULT 0
);
`
	_, err := db.Exec(schema)
	return err
}

// SaveEnvironment 插入或更新环境记录（按 id upsert）。
func (s *Store) SaveEnvironment(rec EnvRecord) error {
	_, err := s.db.Exec(`
INSERT INTO environments (id, name, type, tags, os, arch, capabilities, registered_at, last_seen, online)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(id) DO UPDATE SET
	name = excluded.name, type = excluded.type, tags = excluded.tags,
	os = excluded.os, arch = excluded.arch, capabilities = excluded.capabilities,
	last_seen = excluded.last_seen, online = 1`,
		rec.ID, rec.Name, rec.Type, rec.Tags, rec.OS, rec.Arch, rec.Capabilities,
		rec.RegisteredAt, rec.LastSeen)
	return err
}

// SetEnvironmentOnline 更新环境在线状态。
func (s *Store) SetEnvironmentOnline(id string, online bool) error {
	_, err := s.db.Exec(`UPDATE environments SET online = ?, last_seen = ? WHERE id = ?`,
		boolToInt(online), time.Now(), id)
	return err
}

// ListEnvironments 返回所有持久化的环境（在线状态以 DB 为准，网关启动后应为全离线）。
func (s *Store) ListEnvironments() ([]EnvRecord, error) {
	rows, err := s.db.Query(`SELECT id, name, type, tags, os, arch, capabilities, registered_at, last_seen, online FROM environments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EnvRecord
	for rows.Next() {
		var rec EnvRecord
		var online int
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.Tags, &rec.OS, &rec.Arch,
			&rec.Capabilities, &rec.RegisteredAt, &rec.LastSeen, &online); err != nil {
			return nil, err
		}
		_ = online // 网关重启后内存状态由心跳重建，忽略持久化的 online 值
		out = append(out, rec)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
