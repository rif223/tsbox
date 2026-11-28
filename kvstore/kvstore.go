// Package kvstore is the storage interface behind @tsbox/kv. The engine uses FileStore (one JSON file per
// storage area and script) unless an embedding application passes its own Store with tsbox.WithKVStore,
// e.g. one backed by PostgreSQL or Redis.
//
// A Store keeps one namespace per storage area (BoxSpec.Storage) and script name. The provider of @tsbox/kv
// checks keys, value sizes and TTLs before it calls the store; the store has to provide atomicity per key,
// the version counter, expiry and the quota.
package kvstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Namespace identifies the data of one script in one storage area. Both names are already validated
// ([a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}).
type Namespace struct {
	Storage string `json:"storage"`
	Script  string `json:"script"`
}

// NamespaceInfo describes the data of one namespace, for overviews (GET /v1/kv).
type NamespaceInfo struct {
	Namespace
	Keys     int       `json:"keys"`              // unexpired keys
	Bytes    int64     `json:"bytes"`             // keys + values of the unexpired entries (what counts against the quota)
	Modified time.Time `json:"modified,omitzero"` // last change, if the store knows it
}

// Lister is implemented by stores that can enumerate their namespaces. It is optional: without it, tsboxd
// cannot list the stored data (GET /v1/kv answers "not_supported"), reading one namespace still works.
type Lister interface {
	// Namespaces returns all namespaces that hold data, sorted by storage area and script.
	Namespaces(ctx context.Context) ([]NamespaceInfo, error)
}

// Entry is a stored value.
type Entry struct {
	Value json.RawMessage
	// Version grows with every write of the key (also across delete and re-create in the same namespace);
	// scripts use it for compare-and-set. Stores assign it: a Version returned by an UpdateFunc is ignored.
	Version uint64
	// Expires is the end of the entry's lifetime (zero = never). Expired entries count as absent.
	Expires time.Time
}

// Size is what an entry costs against the quota: key plus value (metadata is free).
func Size(key string, e *Entry) int64 {
	if e == nil {
		return 0
	}
	return int64(len(key) + len(e.Value))
}

// UpdateFunc gets the current entry (nil if absent or expired) and returns the new one (nil = delete,
// the same pointer = leave unchanged). An error aborts the update without changes.
type UpdateFunc func(cur *Entry) (*Entry, error)

// ErrQuota is returned by Update if the namespace would grow beyond its quota.
var ErrQuota = errors.New("kvstore: quota exhausted")

// Store is the storage behind @tsbox/kv. All methods may be called concurrently.
type Store interface {
	// Get returns the entry of a key; ok is false if it is absent or expired.
	Get(ctx context.Context, ns Namespace, key string) (e Entry, ok bool, err error)
	// Update changes one key atomically (read, fn, write) and returns the entry as stored (nil after a
	// delete or when the key stays absent). quota is the byte limit of the whole namespace (keys + values
	// of all unexpired entries); exceeding it gives ErrQuota.
	Update(ctx context.Context, ns Namespace, key string, quota int64, fn UpdateFunc) (*Entry, error)
	// List returns up to limit unexpired keys with the prefix, sorted bytewise, that come after "after"
	// ("" = from the start); more reports whether further keys follow.
	List(ctx context.Context, ns Namespace, prefix, after string, limit int) (keys []string, more bool, err error)
}
