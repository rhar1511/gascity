package beads_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

const observedRevisionTestID = "obs-1"

// Only the bd command transport is scripted. Both routes use the public
// BdStore decoder, and the cache route primes the real CachingStore. No test
// assigns revision presence or marshals a Bead to fabricate a bd envelope.
type observedRevisionTransport struct {
	revision json.RawMessage
	calls    int
	updates  [][]string
	marker   string
}

func (f *observedRevisionTransport) run(_ string, name string, args ...string) ([]byte, error) {
	f.calls++
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected observed revision command %q %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" && slices.Contains([]string{"update", "close", "assign", "delete"}, args[0]) {
		return []byte("--if-revision"), nil
	}
	switch args[0] {
	case "show", "list":
		row := map[string]any{
			"id": observedRevisionTestID, "title": "observed revision", "status": "open", "issue_type": "task",
			"metadata": map[string]string{"marker": f.marker}, "labels": []string{"observed"},
			"dependencies": []any{}, "is_blocked": false,
		}
		if f.revision != nil {
			row["revision"] = f.revision
		}
		return json.Marshal([]any{row})
	case "query":
		// Prime reads both durable and ephemeral tiers; this fixture has one
		// durable row, no wisps, and no dependency edges.
		return []byte("[]"), nil
	case "dep":
		if len(args) > 1 && args[1] == "list" {
			return []byte("[]"), nil
		}
	case "update":
		f.updates = append(f.updates, append([]string(nil), args...))
		// Model the command transport's real precondition semantics, rather
		// than returning the desired conflict unconditionally. A renewed or
		// omitted guard would actually change the fixture marker and fail.
		decimal := string(f.revision)
		if strings.HasPrefix(decimal, `"`) {
			if err := json.Unmarshal(f.revision, &decimal); err != nil {
				return nil, err
			}
		}
		current, err := strconv.ParseInt(decimal, 10, 64)
		if err != nil {
			return nil, err
		}
		if guard := slices.Index(args, "--if-revision"); guard >= 0 {
			if guard+1 >= len(args) {
				return nil, errors.New("missing fixture guard argument")
			}
			expected, err := strconv.ParseInt(args[guard+1], 10, 64)
			if err != nil {
				return nil, err
			}
			if expected != current {
				out, marshalErr := json.Marshal(map[string]any{"code": "precondition_failed", "id": observedRevisionTestID, "expected_revision": expected, "current_revision": current, "mutates": false})
				return out, errors.Join(errors.New("conditional conflict"), marshalErr)
			}
		}
		if metadata := slices.Index(args, "--set-metadata"); metadata >= 0 && metadata+1 < len(args) && args[metadata+1] == "marker=lost" {
			f.marker = "lost"
		}
		f.revision = json.RawMessage(`"-1014862916764651620"`)
		return []byte("[]"), nil
	}
	return nil, fmt.Errorf("unexpected observed revision fixture command %q", args)
}

func observedRevisionStore(t *testing.T, f *observedRevisionTransport, cached bool) (*beads.BdStore, beads.Store) {
	t.Helper()
	f.marker = "untouched"
	backing := beads.NewBdStoreWithPrefix(t.TempDir(), f.run, "obs")
	if !cached {
		return backing, backing
	}
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("prime real cache: %v", err)
	}
	if !cache.IsLive() {
		t.Fatal("cache route must be live, not a cold pass-through")
	}
	return backing, cache
}

func TestObservedRevisionFromBdDetailAndLiveCache(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		wire   json.RawMessage
		want   int64
		usable bool
	}{
		{name: "absent"},
		{name: "null", wire: json.RawMessage(`null`)},
		{name: "published string zero", wire: json.RawMessage(`"0"`), usable: true},
		{name: "published integer zero", wire: json.RawMessage(`0`), usable: true},
		{name: "negative string", wire: json.RawMessage(`"-3285076669762484252"`), want: -3285076669762484252, usable: true},
		{name: "negative integer", wire: json.RawMessage(`-3285076669762484252`), want: -3285076669762484252, usable: true},
		{name: "minimum signed64", wire: json.RawMessage(`"-9223372036854775808"`), want: -9223372036854775808, usable: true},
		{name: "maximum signed64", wire: json.RawMessage(`"9223372036854775807"`), want: 9223372036854775807, usable: true},
		{name: "large string", wire: json.RawMessage(`"9007199254740993"`), want: 9007199254740993, usable: true},
		{name: "large integer", wire: json.RawMessage(`9007199254740993`), want: 9007199254740993, usable: true},
	} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache=%t", tc.name, cached), func(t *testing.T) {
				f := &observedRevisionTransport{revision: tc.wire}
				backing, store := observedRevisionStore(t, f, cached)
				// Exercise the detail envelope independently of the cache's list
				// envelope; both must preserve the same literal token contract.
				detail, err := backing.Get(observedRevisionTestID)
				if err != nil {
					t.Fatal(err)
				}
				if token, usable := beads.ObservedRevision(detail); token != tc.want || usable != tc.usable {
					t.Fatalf("detail token=(%d,%t), want(%d,%t)", token, usable, tc.want, tc.usable)
				}
				beforeGet := f.calls
				got, err := store.Get(observedRevisionTestID)
				if err != nil {
					t.Fatal(err)
				}
				if cached && f.calls != beforeGet {
					t.Fatal("live cache Get fell through to backing transport")
				}
				beforeAccessor := f.calls
				for range 2 {
					if token, usable := beads.ObservedRevision(got); token != tc.want || usable != tc.usable {
						t.Fatalf("observed token=(%d,%t), want(%d,%t)", token, usable, tc.want, tc.usable)
					}
				}
				if f.calls != beforeAccessor || got.Metadata["marker"] != "untouched" || !slices.Equal(got.Labels, []string{"observed"}) || len(f.updates) != 0 {
					t.Fatal("read-only accessor fetched or changed the observed row")
				}
				wire, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				var public map[string]json.RawMessage
				if err := json.Unmarshal(wire, &public); err != nil {
					t.Fatal(err)
				}
				if _, exposed := public["revision"]; exposed {
					t.Fatal("accessor promoted the private revision onto the Bead JSON wire")
				}
				// The next live-cache value copy must keep presence and own its
				// maps/slices even after its caller edits the previous copy.
				got.Metadata["marker"], got.Labels[0] = "caller edit", "caller edit"
				beforeAgain := f.calls
				again, err := store.Get(observedRevisionTestID)
				if err != nil {
					t.Fatal(err)
				}
				if token, usable := beads.ObservedRevision(again); token != tc.want || usable != tc.usable || again.Metadata["marker"] != "untouched" || !slices.Equal(again.Labels, []string{"observed"}) {
					t.Fatalf("next observed copy lost token/presence or ownership: token=(%d,%t)", token, usable)
				}
				if cached && f.calls != beforeAgain {
					t.Fatal("cache clone proof used a backing read")
				}
			})
		}
	}
	if token, usable := beads.ObservedRevision(beads.Bead{}); token != 0 || usable {
		t.Fatal("unpublished native zero became a usable token")
	}
}

func TestObservedRevisionMalformedDetailRefuses(t *testing.T) {
	t.Parallel()
	for _, wire := range []string{`""`, `"bad"`, `"9223372036854775808"`, `-9223372036854775809`, `1.5`, `true`} {
		t.Run(wire, func(t *testing.T) {
			f := &observedRevisionTransport{revision: json.RawMessage(wire)}
			_, store := observedRevisionStore(t, f, false)
			if _, err := store.Get(observedRevisionTestID); err == nil {
				t.Fatal("malformed revision escaped the owning detail decoder")
			}
			if len(f.updates) != 0 {
				t.Fatal("malformed detail attempted a write")
			}
		})
	}
}

func TestObservedRevisionStaleZeroKeepsOriginalGuard(t *testing.T) {
	t.Parallel()
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			f := &observedRevisionTransport{revision: json.RawMessage(`"0"`)}
			_, store := observedRevisionStore(t, f, cached)
			observed, err := store.Get(observedRevisionTestID)
			if err != nil {
				t.Fatal(err)
			}
			token, usable := beads.ObservedRevision(observed)
			if token != 0 || !usable {
				t.Fatal("inspection needs an actually published zero token")
			}
			f.revision = json.RawMessage(`"-3285076669762484252"`)
			writer, ok := beads.ConditionalWriterFor(store)
			if !ok {
				t.Fatal("actual store does not expose conditional writes")
			}
			err = writer.UpdateIfMatch(observedRevisionTestID, token, beads.UpdateOpts{Metadata: map[string]string{"marker": "lost"}})
			var conflict *beads.PreconditionFailedError
			if !errors.As(err, &conflict) || conflict.ID != observedRevisionTestID || conflict.Expected != 0 || conflict.Current != -3285076669762484252 {
				t.Fatalf("stale observed zero was not preserved: %v", err)
			}
			if len(f.updates) != 1 || slices.Index(f.updates[0], "--if-revision") < 0 {
				t.Fatalf("expected one fenced attempt: %q", f.updates)
			}
			guard := slices.Index(f.updates[0], "--if-revision")
			if guard+1 >= len(f.updates[0]) || f.updates[0][guard+1] != "0" {
				t.Fatalf("caller guard was renewed or lost: %q", f.updates)
			}
			fresh, err := store.Get(observedRevisionTestID)
			if err != nil {
				t.Fatal(err)
			}
			if next, present := beads.ObservedRevision(fresh); next != -3285076669762484252 || !present || fresh.Metadata["marker"] != "untouched" {
				t.Fatal("stale refusal wrote the marker or failed to evict/refetch the cache")
			}
		})
	}
}

func TestObservedRevisionLostPresenceRefusesZeroWrite(t *testing.T) {
	t.Parallel()
	for _, lost := range []json.RawMessage{nil, json.RawMessage(`null`)} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("lost=%s/cache=%t", lost, cached), func(t *testing.T) {
				f := &observedRevisionTransport{revision: json.RawMessage(`0`)}
				backing, store := observedRevisionStore(t, f, cached)
				first, err := store.Get(observedRevisionTestID)
				if err != nil {
					t.Fatal(err)
				}
				token, present := beads.ObservedRevision(first)
				if token != 0 || !present {
					t.Fatal("fixture needs published integer zero before presence loss")
				}
				f.revision = lost
				writer, ok := beads.ConditionalWriterFor(store)
				if !ok {
					t.Fatal("actual store does not expose conditional writes")
				}
				err = writer.UpdateIfMatch(observedRevisionTestID, token, beads.UpdateOpts{Metadata: map[string]string{"marker": "lost"}})
				if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || len(f.updates) != 0 {
					t.Fatalf("lost token became a zero write: %v attempts=%q", err, f.updates)
				}
				fresh, err := backing.Get(observedRevisionTestID)
				if err != nil {
					t.Fatal(err)
				}
				if _, usable := beads.ObservedRevision(fresh); usable || fresh.Metadata["marker"] != "untouched" {
					t.Fatal("absent/null detail gained presence or mutation")
				}
			})
		}
	}
}
