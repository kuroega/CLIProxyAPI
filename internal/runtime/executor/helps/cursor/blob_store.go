package cursor

import (
	"fmt"
	"sync"
)

const (
	defaultBlobStoreSessions = 64
	maxBlobSize              = 4 << 20
	maxBlobStoreSize         = 16 << 20
)

// BlobStore holds the bounded blob state that Cursor associates with one conversation.
type BlobStore struct {
	mu   sync.Mutex
	data map[string][]byte
	size int
}

// NewBlobStorePool creates a bounded, process-local pool of conversation blob stores.
func NewBlobStorePool(capacity int) *BlobStorePool {
	if capacity <= 0 {
		capacity = defaultBlobStoreSessions
	}
	return &BlobStorePool{capacity: capacity, stores: make(map[string]*pooledBlobStore, capacity)}
}

// BlobStorePool returns one BlobStore per session and evicts the least recently used session at capacity.
type BlobStorePool struct {
	mu       sync.Mutex
	capacity int
	sequence uint64
	stores   map[string]*pooledBlobStore
}

type pooledBlobStore struct {
	store *BlobStore
	used  uint64
}

// ForSession returns a store for sessionID. Empty session IDs use an isolated shared fallback bucket.
func (p *BlobStorePool) ForSession(sessionID string) *BlobStore {
	if p == nil {
		return &BlobStore{data: make(map[string][]byte)}
	}
	if sessionID == "" {
		sessionID = "default"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	if existing := p.stores[sessionID]; existing != nil {
		existing.used = p.sequence
		return existing.store
	}
	if len(p.stores) >= p.capacity {
		var oldestID string
		var oldestUsed uint64
		for id, entry := range p.stores {
			if oldestID == "" || entry.used < oldestUsed {
				oldestID = id
				oldestUsed = entry.used
			}
		}
		delete(p.stores, oldestID)
	}
	store := &BlobStore{data: make(map[string][]byte)}
	p.stores[sessionID] = &pooledBlobStore{store: store, used: p.sequence}
	return store
}

// Get returns an owned copy of one blob, or nil when the blob does not exist.
func (s *BlobStore) Get(id []byte) []byte {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data[string(id)]...)
}

// Set stores an owned copy of a blob while enforcing per-blob and per-session limits.
func (s *BlobStore) Set(id, data []byte) error {
	if s == nil {
		return fmt.Errorf("cursor blob store is nil")
	}
	if len(data) > maxBlobSize {
		return fmt.Errorf("cursor blob exceeds %d byte limit", maxBlobSize)
	}
	key := string(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := len(s.data[key])
	if s.size-previous+len(data) > maxBlobStoreSize {
		return fmt.Errorf("cursor conversation blobs exceed %d byte limit", maxBlobStoreSize)
	}
	if s.data == nil {
		s.data = make(map[string][]byte)
	}
	s.data[key] = append([]byte(nil), data...)
	s.size = s.size - previous + len(data)
	return nil
}
