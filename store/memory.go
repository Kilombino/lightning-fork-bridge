package store

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/paulscode/lightning-fork-bridge/node"
)

// Memory is a Store that keeps records in memory and loses them on exit.
//
// It exists for tests. A bridge running on one of these would come back from a
// crash believing it had never done anything, and would recover swaps whose
// outgoing legs were already in flight by cancelling the incoming claims, so
// nothing should hand it real money.
type Memory struct {
	mu      sync.Mutex
	records map[node.Hash]Record

	// OnPut, when set, runs before the record is stored. Returning an error
	// makes Put fail, which is how a test makes the disk go away at an exact
	// point in a swap's life.
	OnPut func(Record) error
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{records: map[node.Hash]Record{}}
}

// Put stores a record.
func (m *Memory) Put(ctx context.Context, r Record) error {
	if err := r.Valid(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnPut != nil {
		if err := m.OnPut(r); err != nil {
			return err
		}
	}
	m.records[r.Hash] = r

	return nil
}

// Get returns the current record for a hash.
func (m *Memory) Get(ctx context.Context, hash node.Hash) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.records[hash]
	if !ok {
		return Record{}, fmt.Errorf("%w: %x", ErrNotFound, hash)
	}

	return r, nil
}

// Pending returns every non-terminal record, oldest first.
func (m *Memory) Pending(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Record
	for _, r := range m.records {
		if !r.State.Terminal() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Created.Equal(out[b].Created) {
			return out[a].Created.Before(out[b].Created)
		}

		return bytes.Compare(out[a].Hash[:], out[b].Hash[:]) < 0
	})

	return out, nil
}

// Close does nothing.
func (m *Memory) Close() error { return nil }
