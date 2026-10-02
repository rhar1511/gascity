package beads

import "strings"

// StableCreateIDFor reports whether store makes caller-supplied bead IDs
// unique at the durable store boundary. It follows store wrappers and fails
// closed for stores that do not advertise this capability.
func StableCreateIDFor(store Store) bool {
	if store == nil {
		return false
	}
	store = followStableCreateIDResolveTarget(store)
	creator, ok := store.(stableCreateIDCapability)
	return ok && creator.stableCreateIDSupported()
}

// StableCreateIDPrefixFor returns the durable store's local bead-ID prefix
// when caller-supplied IDs are honored. It follows wrappers the same way as
// StableCreateIDFor and fails closed when the prefix cannot be determined.
func StableCreateIDPrefixFor(store Store) (string, bool) {
	if !StableCreateIDFor(store) {
		return "", false
	}
	store = followStableCreateIDResolveTarget(store)
	var prefix string
	switch current := store.(type) {
	case *MemStore:
		prefix = current.IDPrefix
		if prefix == "" {
			prefix = "gc"
		}
	default:
		provider, ok := store.(interface{ IDPrefix() string })
		if !ok {
			return "", false
		}
		prefix = provider.IDPrefix()
	}
	prefix = normalizeIDPrefix(prefix)
	if prefix == "" || strings.ContainsAny(prefix, " \t\r\n") {
		return "", false
	}
	return prefix, true
}

// StableCreateIDResolveTargeter lets wrappers expose only their durable
// create-ID target without changing resolution semantics for unrelated write
// capabilities such as cached metadata CAS.
type StableCreateIDResolveTargeter interface {
	StableCreateIDResolveTarget() Store
}

func followStableCreateIDResolveTarget(store Store) Store {
	for range conditionalWritesMaxResolveDepth {
		targeter, ok := store.(StableCreateIDResolveTargeter)
		if !ok {
			return store
		}
		target := targeter.StableCreateIDResolveTarget()
		if target == nil || target == store {
			return store
		}
		store = target
	}
	return store
}

type stableCreateIDCapability interface {
	stableCreateIDSupported() bool
}

func (*BdStore) stableCreateIDSupported() bool         { return true }
func (*SQLiteStore) stableCreateIDSupported() bool     { return true }
func (*NativeDoltStore) stableCreateIDSupported() bool { return true }

func (s *MemStore) stableCreateIDSupported() bool {
	return s != nil && s.HonorExplicitIDs
}
