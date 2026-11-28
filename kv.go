package tsbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rif223/tsbox/kvstore"
)

// Read access to the data of @tsbox/kv for operators (tsboxd: GET /v1/kv), without going through a script.

// ErrKVUnavailable: the kv provider was replaced by an embedder, so the engine does not know its store.
var ErrKVUnavailable = errors.New("kv data is not available: the kv provider was replaced")

// ErrKVNotListable: the store cannot enumerate its namespaces (it does not implement kvstore.Lister).
var ErrKVNotListable = errors.New("the kv store cannot list its namespaces")

// ErrKVInvalidName: storage area or script name break the rule [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}.
var ErrKVInvalidName = errors.New("invalid kv namespace")

// MaxKVPage is the largest number of entries KVEntries returns at once.
const MaxKVPage = 500

var kvName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// KVEntry is a stored value as an operator sees it.
type KVEntry struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version uint64          `json:"version"`
	Expires time.Time       `json:"expires,omitzero"`
}

// KVNamespaces lists the namespaces (storage area and script) that hold data.
func (e *Engine) KVNamespaces(ctx context.Context) ([]kvstore.NamespaceInfo, error) {
	if e.kv == nil {
		return nil, ErrKVUnavailable
	}
	l, ok := e.kv.(kvstore.Lister)
	if !ok {
		return nil, ErrKVNotListable
	}
	return l.Namespaces(ctx)
}

// KVEntries returns up to limit unexpired entries of a namespace in key order, starting after the key
// "after" ("" = from the start); more reports whether further entries follow. limit ≤ 0 means 100; at most
// MaxKVPage.
func (e *Engine) KVEntries(ctx context.Context, ns kvstore.Namespace, after string, limit int) (entries []KVEntry, more bool, err error) {
	if e.kv == nil {
		return nil, false, ErrKVUnavailable
	}
	if !kvName.MatchString(ns.Storage) || !kvName.MatchString(ns.Script) {
		return nil, false, fmt.Errorf("%w %q/%q", ErrKVInvalidName, ns.Storage, ns.Script)
	}
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, MaxKVPage)
	keys, more, err := e.kv.List(ctx, ns, "", after, limit)
	if err != nil {
		return nil, false, err
	}
	entries = make([]KVEntry, 0, len(keys))
	for _, k := range keys {
		en, ok, err := e.kv.Get(ctx, ns, k)
		if err != nil {
			return nil, false, err
		}
		if ok { // expired or deleted between List and Get otherwise
			entries = append(entries, KVEntry{Key: k, Value: en.Value, Version: en.Version, Expires: en.Expires})
		}
	}
	return entries, more, nil
}
