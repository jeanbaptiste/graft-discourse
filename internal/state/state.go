package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// dedupRetention bounds how long an entry survives in the Delivered,
// PostsDelivered and SeenNotes sets before it's pruned. Nothing ever
// removed entries before, so a long-running bridge's state.json grew
// without bound; 180 days is far past any realistic window for a
// Discourse post or Graft note to still be "in flight" for dedup
// purposes, so pruning past it can't cause a duplicate delivery in
// practice while still keeping the file bounded over the bridge's
// lifetime.
const dedupRetention = 180 * 24 * time.Hour

// KeyPair is a bridge actor's PEM keypair.
type KeyPair struct {
	PrivatePEM string `json:"private_pem"`
	PublicPEM  string `json:"public_pem"`
}

// State is the bridge's durable bookkeeping, persisted as a JSON file. It
// is deliberately small: identity keys, the topic<->issue mapping, and the
// dedup sets that keep the two sync directions from looping.
type State struct {
	mu   sync.Mutex
	path string

	ActorKeys   map[string]KeyPair `json:"actor_keys"`
	TopicNotes  map[string]string  `json:"topic_notes"`  // Discourse topic id -> Graft issue/patch note URI
	IssueTopics map[string]string  `json:"issue_topics"` // "series|issueURL" -> Discourse topic id

	// Delivered, PostsDelivered and SeenNotes are dedup sets keyed by the
	// unix-second timestamp an entry was first recorded, not just a bare
	// bool, so saveLocked can prune anything older than dedupRetention
	// instead of growing these forever.
	Delivered      map[string]int64 `json:"delivered"`       // content hash of a Discourse post already sent to Graft
	PostsDelivered map[string]int64 `json:"posts_delivered"` // "series|postID" already sent to Graft
	SeenNotes      map[string]int64 `json:"seen_notes"`      // "series|noteURI" already mirrored to Discourse
}

// Load reads state from path, creating an empty state if the file is absent.
// Because the file holds the actor's private key, a group- or
// world-accessible file is refused outright rather than silently used.
func Load(path string) (*State, error) {
	s := &State{path: path}
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf("state file %s is accessible to other users (mode %04o); run: chmod 600 %s", path, mode, path)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		s.initMaps()
		return s, nil
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	s.initMaps()
	return s, nil
}

func (s *State) initMaps() {
	if s.ActorKeys == nil {
		s.ActorKeys = map[string]KeyPair{}
	}
	if s.TopicNotes == nil {
		s.TopicNotes = map[string]string{}
	}
	if s.IssueTopics == nil {
		s.IssueTopics = map[string]string{}
	}
	if s.Delivered == nil {
		s.Delivered = map[string]int64{}
	}
	if s.PostsDelivered == nil {
		s.PostsDelivered = map[string]int64{}
	}
	if s.SeenNotes == nil {
		s.SeenNotes = map[string]int64{}
	}
}

// prune drops dedup entries older than dedupRetention. Called from
// saveLocked, under s.mu, before every write.
func (s *State) prune() {
	cutoff := time.Now().Add(-dedupRetention).Unix()
	for _, m := range []map[string]int64{s.Delivered, s.PostsDelivered, s.SeenNotes} {
		for k, ts := range m {
			if ts < cutoff {
				delete(m, k)
			}
		}
	}
}

// Save atomically writes the state back to disk.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *State) saveLocked() error {
	s.prune()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// The state file holds the actor's private key and mapping bookkeeping;
	// keep it owner-only regardless of umask.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}

// KeyPair returns the stored keypair for name.
func (s *State) KeyPair(name string) (KeyPair, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp, ok := s.ActorKeys[name]
	return kp, ok
}

// SetKeyPair stores name's keypair and persists.
func (s *State) SetKeyPair(name string, kp KeyPair) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ActorKeys[name] = kp
	return s.saveLocked()
}

// TopicNote returns the Graft note URI mapped to a Discourse topic.
func (s *State) TopicNote(topicID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.TopicNotes[strconv.FormatInt(topicID, 10)]
	return v, ok
}

// SetTopicNote records a Discourse topic -> Graft note URI mapping.
func (s *State) SetTopicNote(topicID int64, noteURI string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TopicNotes[strconv.FormatInt(topicID, 10)] = noteURI
	return s.saveLocked()
}

// DeleteTopicNote removes a topic's mapping and any issue-URL links that
// point at it, and persists. It is a no-op if the topic was not mapped.
func (s *State) DeleteTopicNote(topicID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strconv.FormatInt(topicID, 10)
	delete(s.TopicNotes, key)
	for k, v := range s.IssueTopics {
		if v == key {
			delete(s.IssueTopics, k)
		}
	}
	return s.saveLocked()
}

// AllTopicNotes returns a copy of the topic -> note URI mappings.
func (s *State) AllTopicNotes() map[int64]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]string, len(s.TopicNotes))
	for k, v := range s.TopicNotes {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue
		}
		out[id] = v
	}
	return out
}

// IssueTopic returns the Discourse topic mapped to a repo issue URL.
func (s *State) IssueTopic(series, issueURL string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.IssueTopics[series+"|"+issueURL]
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// SetIssueTopic records a repo issue URL -> Discourse topic mapping.
func (s *State) SetIssueTopic(series, issueURL string, topicID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IssueTopics[series+"|"+issueURL] = strconv.FormatInt(topicID, 10)
	return s.saveLocked()
}

// IsDelivered reports whether a Discourse post hash was already sent.
func (s *State) IsDelivered(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Delivered[hash]
	return ok
}

// MarkDelivered records a Discourse post hash and persists.
func (s *State) MarkDelivered(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Delivered[hash] = time.Now().Unix()
	return s.saveLocked()
}

// PostDelivered reports whether a specific Discourse post already reached
// Graft, keyed on its stable id (not its content, which a user can change).
func (s *State) PostDelivered(series string, postID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.PostsDelivered[series+"|"+strconv.FormatInt(postID, 10)]
	return ok
}

// MarkPostDelivered records a Discourse post id as sent and persists.
func (s *State) MarkPostDelivered(series string, postID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PostsDelivered[series+"|"+strconv.FormatInt(postID, 10)] = time.Now().Unix()
	return s.saveLocked()
}

// SeenNote reports whether a Graft note was already mirrored to Discourse.
func (s *State) SeenNote(series, noteURI string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.SeenNotes[series+"|"+noteURI]
	return ok
}

// MarkSeenNote records a Graft note as mirrored and persists.
func (s *State) MarkSeenNote(series, noteURI string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SeenNotes[series+"|"+noteURI] = time.Now().Unix()
	return s.saveLocked()
}
