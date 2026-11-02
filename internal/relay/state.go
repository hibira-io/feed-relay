package relay

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// keepSeen is how many item IDs are remembered per feed. Feeds rarely list
// more than 100 entries, so an item that drops out of this window has also
// dropped out of the feed.
const keepSeen = 1000

// FeedState is what the relay remembers about one feed between runs.
type FeedState struct {
	Primed       bool      `json:"primed"`
	Seen         []string  `json:"seen"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Hub          string    `json:"hub,omitempty"`
	Topic        string    `json:"topic,omitempty"`
	LeaseUntil   time.Time `json:"lease_until,omitzero"`
	RetryAt      time.Time `json:"retry_at,omitzero"`
}

// Store keeps every feed's state in one JSON file.
type Store struct {
	path  string
	mu    sync.Mutex
	feeds map[string]*FeedState
	sets  map[string]map[string]bool
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, feeds: map[string]*FeedState{}, sets: map[string]map[string]bool{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.feeds); err != nil {
		return nil, err
	}
	for name, st := range s.feeds {
		set := make(map[string]bool, len(st.Seen))
		for _, id := range st.Seen {
			set[id] = true
		}
		s.sets[name] = set
	}
	return s, nil
}

// Get returns a copy of the feed's state.
func (s *Store) Get(name string) FeedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.feeds[name]; st != nil {
		c := *st
		c.Seen = nil
		return c
	}
	return FeedState{}
}

func (s *Store) Seen(name, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sets[name][id]
}

// Update changes the feed's state under the lock.
func (s *Store) Update(name string, fn func(*FeedState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.feeds[name]
	if st == nil {
		st = &FeedState{}
		s.feeds[name] = st
	}
	fn(st)
}

// MarkSeen remembers ids, dropping the oldest past keepSeen.
func (s *Store) MarkSeen(name string, ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.feeds[name]
	if st == nil {
		st = &FeedState{}
		s.feeds[name] = st
	}
	set := s.sets[name]
	if set == nil {
		set = map[string]bool{}
		s.sets[name] = set
	}
	for _, id := range ids {
		if !set[id] {
			set[id] = true
			st.Seen = append(st.Seen, id)
		}
	}
	if over := len(st.Seen) - keepSeen; over > 0 {
		for _, id := range st.Seen[:over] {
			delete(set, id)
		}
		st.Seen = append([]string(nil), st.Seen[over:]...)
	}
}

// Save writes the file atomically.
func (s *Store) Save() error {
	s.mu.Lock()
	raw, err := json.MarshalIndent(s.feeds, "", " ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".feed-relay-state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
