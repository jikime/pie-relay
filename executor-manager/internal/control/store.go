package control

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrConflict = errors.New("control record version conflict")

type Record struct {
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Version int64           `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// Store is deliberately record-oriented. The Directory implementation keeps
// standalone development simple while PostgreSQL can enforce the same
// optimistic version contract across several manager processes.
type Store interface {
	Load(context.Context) ([]Record, error)
	Put(context.Context, Record, int64) error
	Delete(context.Context, string, string, int64) error
	Ping(context.Context) error
	Close() error
}

// DistributedLocker serializes scheduler decisions across Manager replicas.
// DirectoryStore intentionally does not implement it because that store is
// only supported by a single standalone Manager process.
type DistributedLocker interface {
	WithLock(context.Context, string, func() error) error
}

// SharedStore marks stores that can be modified by another Manager process.
// Standalone directory stores skip periodic full reloads entirely.
type SharedStore interface{ Shared() bool }

// CurrentStore can bound append-only history while loading the operational
// snapshot. Audit records remain durable in PostgreSQL and are queried through
// the API, while the hot in-memory view keeps only the recent window.
type CurrentStore interface {
	LoadCurrent(context.Context, int) ([]Record, error)
}

// RecordChange is a durable mutation cursor for shared stores. Record contains
// the latest current value for Kind/ID; Deleted is true when no value remains.
// Returning the latest value (rather than historical JSON) keeps the change log
// compact and makes replay idempotent when one object changes many times.
type RecordChange struct {
	Sequence int64
	Record   Record
	Deleted  bool
}

// IncrementalStore lets a multi-Manager deployment synchronize only records
// changed since its last reconciliation instead of reloading the full control
// snapshot every few seconds.
type IncrementalStore interface {
	SharedStore
	CurrentSequence(context.Context) (int64, error)
	LoadChanges(context.Context, int64, int) ([]RecordChange, error)
}

func makeRecord(kind, id string, version int64, value any) (Record, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Record{}, err
	}
	return Record{Kind: kind, ID: id, Version: version, Data: data}, nil
}
