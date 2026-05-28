// Embedding publish-hook tests exercise the full V2 dispatcher
// emission pipeline (publish-after-contains-edge ordering,
// per-method/per-block publish, content threading). Un-gated
// in iter 6 when the canonical dispatcher landed the publisher
// hook (see dispatcher.go::publish).
package ast

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// recordingPublisher captures every PublishNodeEmbedding call
// in source order so tests can assert kind, content, and
// publish-after-edge ordering.  An optional `errOn` map lets a
// test inject a sentinel for the Nth call.
type recordingPublisher struct {
	mu        sync.Mutex
	calls     []NodeEmbedRequest
	errOnKind map[string]error
}

func newRecordingPublisher() *recordingPublisher {
	return &recordingPublisher{errOnKind: map[string]error{}}
}

func (r *recordingPublisher) PublishNodeEmbedding(_ context.Context, req NodeEmbedRequest) (NodeEmbedResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	if err, ok := r.errOnKind[req.Kind]; ok {
		return NodeEmbedResult{PublishID: "p-fake", LastEventKind: "failed"}, err
	}
	return NodeEmbedResult{PublishID: "p-fake", LastEventKind: "published"}, nil
}

func (r *recordingPublisher) snapshot() []NodeEmbedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NodeEmbedRequest, len(r.calls))
	copy(out, r.calls)
	return out
}

func countKind(reqs []NodeEmbedRequest, kind string) int {
	n := 0
	for _, r := range reqs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// TestDispatcher_EmbeddingPublisher_FiresForMethodsAndBlocks
// asserts the §9.6a publish hook is invoked once per Method
// node AND once per Block node the dispatcher emits.
// Locks in:
//
//   - the per-Method invocation (Pass 1b → publish hook)
//   - the per-Block invocation (Pass 1c → publish hook)
//   - publisher Kind matches the Node kind it follows
//   - the Method publish carries non-empty Content (the
//     parser's BodySource — empty would defeat embedding)
//   - the dispatcher does NOT publish for Class or Package
//     nodes (those surfaces are owned by other publishers)
func TestDispatcher_EmbeddingPublisher_FiresForMethodsAndBlocks(t *testing.T) {
	fw := newFakeWriter()
	rp := newRecordingPublisher()
	d := NewDispatcher(fw, WithEmbeddingPublisher(rp))

	src := "class Foo { bar() { return 1; } baz() { if (true) { return 2; } } }"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/a.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	calls := rp.snapshot()
	methods := countKind(calls, "method")
	blocks := countKind(calls, "block")
	classes := countKind(calls, "class")

	emittedMethods := len(fw.nodesOf("method"))
	emittedBlocks := len(fw.nodesOf("block"))
	if methods != emittedMethods {
		t.Errorf("publisher saw %d method publishes; emitter inserted %d method nodes",
			methods, emittedMethods)
	}
	if blocks != emittedBlocks {
		t.Errorf("publisher saw %d block publishes; emitter inserted %d block nodes",
			blocks, emittedBlocks)
	}
	if classes != 0 {
		t.Errorf("publisher saw %d class publishes; want 0 (classes are not published)", classes)
	}

	// Every method publish MUST carry non-empty Content.
	// Empty content silently degrades embedding quality and
	// would mask a parser bug.
	for i, c := range calls {
		if c.Kind == "method" && strings.TrimSpace(c.Content) == "" {
			t.Errorf("calls[%d] (method %q) had empty Content", i, c.CanonicalSignature)
		}
		if c.NodeID == "" {
			t.Errorf("calls[%d] (kind %q) had empty NodeID", i, c.Kind)
		}
		if c.RepoID == "" {
			t.Errorf("calls[%d] (kind %q) had empty RepoID", i, c.Kind)
		}
		if c.CanonicalSignature == "" {
			t.Errorf("calls[%d] (kind %q) had empty CanonicalSignature", i, c.Kind)
		}
	}
}

// TestDispatcher_EmbeddingPublisher_PublishesAfterContainsEdge
// locks rubber-duck #5: the publisher fires AFTER the
// `contains` edge is committed, never before.  We assert by
// comparing call indexes against the fake writer's edge log
// (the edge insert happens immediately before the publish for
// the same Node, so the edge count at publish time should
// match the number of method+block contains edges inserted so
// far).
func TestDispatcher_EmbeddingPublisher_PublishesAfterContainsEdge(t *testing.T) {
	fw := newFakeWriter()
	calledAfter := []int{} // edges count at the moment publish fires
	publisher := &orderingPublisher{
		onCall: func(req NodeEmbedRequest) {
			calledAfter = append(calledAfter, len(fw.edgesOf("contains")))
		},
	}
	d := NewDispatcher(fw, WithEmbeddingPublisher(publisher))

	src := "class Foo { bar() { return 1; } }"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/a.ts", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	// Each publish call MUST see at least 1 contains edge
	// already in the writer (the parent->self edge that was
	// just committed).  A publish that fired BEFORE its
	// edge would see fewer edges than the loop iteration
	// index suggests.
	for i, edgeCount := range calledAfter {
		if edgeCount == 0 {
			t.Errorf("publish call %d fired with 0 contains edges committed; "+
				"violates rubber-duck #5 (must publish AFTER contains edge)", i)
		}
	}
	if len(calledAfter) == 0 {
		t.Fatalf("no publish calls fired; test fixture broken")
	}
}

// orderingPublisher is a minimal NodeEmbeddingPublisher that
// invokes a hook on every call.  Used by the
// publishes-after-contains-edge test above.
type orderingPublisher struct {
	onCall func(NodeEmbedRequest)
}

func (o *orderingPublisher) PublishNodeEmbedding(_ context.Context, req NodeEmbedRequest) (NodeEmbedResult, error) {
	if o.onCall != nil {
		o.onCall(req)
	}
	return NodeEmbedResult{}, nil
}

// TestDispatcher_EmbeddingPublisher_RecordedFailedIsTolerated
// locks the two-bucket error policy from `ast/embedding.go`:
// an error wrapped in `ErrPublishRecordedFailed` (i.e. the
// publisher already recorded a `failed` event) MUST NOT
// abort the ingest.  A NON-recorded error (e.g. PG outage)
// MUST propagate.
func TestDispatcher_EmbeddingPublisher_RecordedFailedIsTolerated(t *testing.T) {
	t.Run("recorded failed is swallowed", func(t *testing.T) {
		fw := newFakeWriter()
		rp := newRecordingPublisher()
		rp.errOnKind["method"] = errors.Join(ErrPublishRecordedFailed, errors.New("qdrant 503"))
		d := NewDispatcher(fw, WithEmbeddingPublisher(rp))

		// Two methods so the second invocation proves the
		// dispatcher CONTINUED after the first method's
		// recorded-failed publish.
		src := "class Foo { bar() { return 1; } baz() { return 2; } }"
		_, err := d.EmitFile(context.Background(), makeEvent("src/a.ts", src))
		if err != nil {
			t.Fatalf("EmitFile should swallow ErrPublishRecordedFailed; got %v", err)
		}
		methods := countKind(rp.snapshot(), "method")
		if methods < 2 {
			t.Errorf("expected dispatcher to attempt all method publishes after "+
				"recorded-failed; got %d method calls", methods)
		}
	})

	t.Run("non-recorded error aborts", func(t *testing.T) {
		fw := newFakeWriter()
		rp := newRecordingPublisher()
		rp.errOnKind["method"] = errors.New("pg connection refused")
		d := NewDispatcher(fw, WithEmbeddingPublisher(rp))

		src := "class Foo { bar() { return 1; } baz() { return 2; } }"
		_, err := d.EmitFile(context.Background(), makeEvent("src/a.ts", src))
		if err == nil {
			t.Fatalf("EmitFile should propagate non-recorded errors; got nil")
		}
	})
}

// TestDispatcher_EmbeddingPublisher_DefaultIsNoOp guarantees
// the dispatcher still works when constructed WITHOUT
// WithEmbeddingPublisher — every existing test in
// dispatcher_test.go relies on that behaviour.
func TestDispatcher_EmbeddingPublisher_DefaultIsNoOp(t *testing.T) {
	fw := newFakeWriter()
	d := NewDispatcher(fw) // no WithEmbeddingPublisher
	src := "class Foo { bar() { return 1; } }"
	if _, err := d.EmitFile(context.Background(), makeEvent("src/a.ts", src)); err != nil {
		t.Fatalf("EmitFile (no publisher): %v", err)
	}
}

// Verify the noop publisher satisfies the interface contract.
var _ NodeEmbeddingPublisher = noopNodeEmbeddingPublisher{}
var _ NodeEmbeddingPublisher = (*recordingPublisher)(nil)
var _ NodeEmbeddingPublisher = (*orderingPublisher)(nil)
