package store

import (
	"database/sql"
	"time"

	"github.com/TouwaStar/BlackLight/pkg/model"
	_ "github.com/mattn/go-sqlite3"
)

const bucketSize = 5 * 60 // 5 minutes in seconds

// Store persists traffic observations and node activity in SQLite.
type Store struct {
	db *sql.DB
}

// NodeStat holds historical activity data for a single node.
type NodeStat struct {
	NodeID      string `json:"node_id"`
	FirstSeen   int64  `json:"first_seen"`   // unix epoch
	LastTraffic int64  `json:"last_traffic"` // unix epoch, 0 if never
	TotalConns  int64  `json:"total_conns"`
}

// Bucket is one time-series data point for edge history.
type Bucket struct {
	Timestamp int64 `json:"timestamp"` // unix epoch (bucket start)
	Conns     int   `json:"conns"`
	Errors    int   `json:"errors"`
}

// New opens (or creates) the SQLite database at dbPath and runs migrations.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS traffic_log (
			bucket  INTEGER NOT NULL,
			source  TEXT NOT NULL,
			target  TEXT NOT NULL,
			conns   INTEGER NOT NULL,
			errors  INTEGER NOT NULL,
			PRIMARY KEY (bucket, source, target)
		);
		CREATE TABLE IF NOT EXISTS node_activity (
			node_id      TEXT PRIMARY KEY,
			first_seen   INTEGER NOT NULL,
			last_traffic INTEGER NOT NULL,
			total_conns  INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS node_positions (
			node_id TEXT PRIMARY KEY,
			x       REAL NOT NULL,
			y       REAL NOT NULL
		);
		CREATE TABLE IF NOT EXISTS traffic_edges (
			from_id TEXT NOT NULL,
			to_id   TEXT NOT NULL,
			kind    TEXT NOT NULL,
			detail  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (from_id, to_id)
		);
	`)
	return err
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// RecordTraffic persists a traffic snapshot into 5-minute buckets and updates node activity.
func (s *Store) RecordTraffic(snap *model.TrafficSnapshot) error {
	if snap == nil || len(snap.Connections) == 0 {
		return nil
	}

	now := time.Now().Unix()
	bucket := (now / bucketSize) * bucketSize

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	trafficStmt, err := tx.Prepare(`
		INSERT INTO traffic_log (bucket, source, target, conns, errors)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(bucket, source, target) DO UPDATE SET
			conns = MAX(conns, excluded.conns),
			errors = MAX(errors, excluded.errors)
	`)
	if err != nil {
		return err
	}
	defer trafficStmt.Close()

	activityStmt, err := tx.Prepare(`
		INSERT INTO node_activity (node_id, first_seen, last_traffic, total_conns)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			last_traffic = MAX(last_traffic, excluded.last_traffic),
			total_conns = total_conns + excluded.total_conns
	`)
	if err != nil {
		return err
	}
	defer activityStmt.Close()

	for _, c := range snap.Connections {
		if _, err := trafficStmt.Exec(bucket, c.SourceWorkload, c.TargetService, c.ConnCount, c.ErrorCount); err != nil {
			return err
		}

		// Update source and target node activity.
		if c.ConnCount > 0 || c.ErrorCount > 0 {
			if _, err := activityStmt.Exec(c.SourceWorkload, now, now, c.ConnCount); err != nil {
				return err
			}
			if _, err := activityStmt.Exec(c.TargetService, now, now, c.ConnCount); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// RecordNodes ensures all discovered nodes exist in node_activity.
// New nodes get first_seen = now, existing nodes are left unchanged.
func (s *Store) RecordNodes(nodes []model.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO node_activity (node_id, first_seen, last_traffic, total_conns)
		VALUES (?, ?, 0, 0)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, n := range nodes {
		if _, err := stmt.Exec(n.ID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NodeStats returns activity data for all known nodes.
func (s *Store) NodeStats() ([]NodeStat, error) {
	rows, err := s.db.Query(`SELECT node_id, first_seen, last_traffic, total_conns FROM node_activity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []NodeStat
	for rows.Next() {
		var ns NodeStat
		if err := rows.Scan(&ns.NodeID, &ns.FirstSeen, &ns.LastTraffic, &ns.TotalConns); err != nil {
			return nil, err
		}
		stats = append(stats, ns)
	}
	return stats, rows.Err()
}

// Position is a saved node position.
type Position struct {
	NodeID string  `json:"node_id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
}

// SavePositions persists node layout positions.
func (s *Store) SavePositions(positions []Position) error {
	if len(positions) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO node_positions (node_id, x, y) VALUES (?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET x = excluded.x, y = excluded.y
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range positions {
		if _, err := stmt.Exec(p.NodeID, p.X, p.Y); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadPositions returns all saved node positions.
func (s *Store) LoadPositions() ([]Position, error) {
	rows, err := s.db.Query(`SELECT node_id, x, y FROM node_positions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []Position
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.NodeID, &p.X, &p.Y); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

// SaveTrafficEdges persists traffic-discovered edges.
func (s *Store) SaveTrafficEdges(edges []model.Edge) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM traffic_edges`); err != nil {
		return err
	}

	if len(edges) == 0 {
		return tx.Commit()
	}

	stmt, err := tx.Prepare(`INSERT INTO traffic_edges (from_id, to_id, kind, detail) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range edges {
		if _, err := stmt.Exec(e.From, e.To, e.Kind, e.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadTrafficEdges returns all persisted traffic-discovered edges.
func (s *Store) LoadTrafficEdges() ([]model.Edge, error) {
	rows, err := s.db.Query(`SELECT from_id, to_id, kind, detail FROM traffic_edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []model.Edge
	for rows.Next() {
		var e model.Edge
		if err := rows.Scan(&e.From, &e.To, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// EdgeHistory returns time-series buckets for a specific edge since the given time.
func (s *Store) EdgeHistory(source, target string, since time.Time) ([]Bucket, error) {
	sinceBucket := (since.Unix() / bucketSize) * bucketSize
	rows, err := s.db.Query(`
		SELECT bucket, conns, errors FROM traffic_log
		WHERE source = ? AND target = ? AND bucket >= ?
		ORDER BY bucket ASC
	`, source, target, sinceBucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Timestamp, &b.Conns, &b.Errors); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}
