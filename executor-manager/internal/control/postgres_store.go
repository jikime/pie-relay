package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type PostgresStore struct {
	db        *sql.DB
	lockSlots chan struct{}
}

func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	store := &PostgresStore{db: db, lockSlots: make(chan struct{}, 8)}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS pie_control_records (
  kind text NOT NULL,
  id text NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  data jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kind, id)
);
CREATE INDEX IF NOT EXISTS pie_control_records_kind_updated_idx
  ON pie_control_records (kind, updated_at DESC);
CREATE TABLE IF NOT EXISTS pie_control_changes (
  sequence bigserial NOT NULL UNIQUE,
  kind text NOT NULL,
  id text NOT NULL,
  version bigint NOT NULL,
  deleted boolean NOT NULL,
  changed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kind, id)
);
CREATE OR REPLACE FUNCTION pie_control_record_changed() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    INSERT INTO pie_control_changes(kind,id,version,deleted)
      VALUES(OLD.kind,OLD.id,OLD.version,true)
      ON CONFLICT(kind,id) DO UPDATE SET
        sequence=EXCLUDED.sequence,version=EXCLUDED.version,
        deleted=true,changed_at=now();
  ELSE
    INSERT INTO pie_control_changes(kind,id,version,deleted)
      VALUES(NEW.kind,NEW.id,NEW.version,false)
      ON CONFLICT(kind,id) DO UPDATE SET
        sequence=EXCLUDED.sequence,version=EXCLUDED.version,
        deleted=false,changed_at=now();
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_trigger
    WHERE tgname = 'pie_control_records_changed'
      AND tgrelid = 'pie_control_records'::regclass
  ) THEN
    BEGIN
      CREATE TRIGGER pie_control_records_changed
        AFTER INSERT OR UPDATE OR DELETE ON pie_control_records
        FOR EACH ROW EXECUTE FUNCTION pie_control_record_changed();
    EXCEPTION WHEN duplicate_object THEN
      NULL;
    END;
  END IF;
END;
$$;`)
	return err
}

func (s *PostgresStore) Load(ctx context.Context) ([]Record, error) {
	return s.LoadCurrent(ctx, 2000)
}

func (s *PostgresStore) LoadCurrent(ctx context.Context, auditLimit int) ([]Record, error) {
	if auditLimit < 1 {
		auditLimit = 2000
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, id, version, data FROM pie_control_records WHERE kind <> 'audit'
UNION ALL
SELECT kind, id, version, data FROM (
  SELECT kind, id, version, data FROM pie_control_records
  WHERE kind = 'audit' ORDER BY updated_at DESC LIMIT $1
) recent_audit
ORDER BY kind, id`, auditLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var record Record
		if err := rows.Scan(&record.Kind, &record.ID, &record.Version, &record.Data); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Shared() bool { return true }

// WithLock holds a transaction-scoped PostgreSQL advisory lock while fn makes
// a cross-replica scheduling decision. fn persists through the normal pool;
// this transaction exists only as an automatically released lock lease.
func (s *PostgresStore) WithLock(ctx context.Context, key string, fn func() error) error {
	if s == nil || s.db == nil || strings.TrimSpace(key) == "" || fn == nil {
		return errors.New("invalid PostgreSQL distributed lock")
	}
	select {
	case s.lockSlots <- struct{}{}:
		defer func() { <-s.lockSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) CurrentSequence(ctx context.Context) (int64, error) {
	var sequence int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM pie_control_changes`).Scan(&sequence); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (s *PostgresStore) LoadChanges(ctx context.Context, after int64, limit int) ([]RecordChange, error) {
	if after < 0 {
		return nil, errors.New("invalid control change cursor")
	}
	if limit < 1 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT c.sequence,c.kind,c.id,COALESCE(r.version,0),r.data,(r.id IS NULL)
FROM pie_control_changes c
LEFT JOIN pie_control_records r ON r.kind=c.kind AND r.id=c.id
WHERE c.sequence > $1
ORDER BY c.sequence
LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordChange
	for rows.Next() {
		var change RecordChange
		var data []byte
		if err := rows.Scan(&change.Sequence, &change.Record.Kind, &change.Record.ID, &change.Record.Version, &data, &change.Deleted); err != nil {
			return nil, err
		}
		if !change.Deleted {
			change.Record.Data = data
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Put(ctx context.Context, record Record, expectedVersion int64) error {
	if !validRecordPart(record.Kind) || !validRecordPart(record.ID) || record.Version != expectedVersion+1 || len(record.Data) == 0 {
		return errors.New("invalid control record")
	}
	var result sql.Result
	var err error
	if expectedVersion == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO pie_control_records(kind,id,version,data) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, record.Kind, record.ID, record.Version, record.Data)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE pie_control_records SET version=$3,data=$4,updated_at=now() WHERE kind=$1 AND id=$2 AND version=$5`, record.Kind, record.ID, record.Version, record.Data, expectedVersion)
	}
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresStore) Delete(ctx context.Context, kind, id string, expectedVersion int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM pie_control_records WHERE kind=$1 AND id=$2 AND version=$3`, kind, id, expectedVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

func (s *PostgresStore) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres store is not open")
	}
	return s.db.PingContext(ctx)
}

func (s *PostgresStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
