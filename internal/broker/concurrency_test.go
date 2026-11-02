package broker

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"agentmux/internal/wire"
)

// All harnesses deliberately reuse the same JSON-RPC IDs. Concurrent writes
// and out-of-order upstream responses must retain client ownership instead of
// losing, duplicating or cross-delivering responses.
func TestConcurrentStatelessHarnessRequestsDoNotCollide(t *testing.T) {
	const harnesses, requests = 8, 6
	b, transport := setupBackend(t, false, 10*time.Second)
	peers := make([]testPeer, harnesses)
	for i := range peers {
		peers[i] = attachTestPeer(t, b)
	}
	var writers sync.WaitGroup
	for i := range peers {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			for id := 1; id <= requests; id++ {
				name := fmt.Sprintf("harness-%d-request-%d", i, id)
				b.handle(peers[i].peer, modernMessage(id, "tools/call", map[string]any{"name": name}))
			}
		}(i)
	}
	writers.Wait()

	seenUpstream := make(map[string]bool)
	forwarded := make([]wire.Message, 0, harnesses*requests)
	for len(forwarded) < harnesses*requests {
		message := nextWrite(t, transport)
		key := string(message.ID())
		if seenUpstream[key] {
			t.Fatalf("upstream request ID collision: %s", key)
		}
		seenUpstream[key] = true
		forwarded = append(forwarded, message)
	}
	// Reverse reply order to make client routing independent of request order.
	for i := len(forwarded) - 1; i >= 0; i-- {
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(forwarded[i]["params"], &params); err != nil {
			t.Fatal(err)
		}
		transport.reads <- wire.Result(forwarded[i].ID(), map[string]string{"owner": params.Name})
	}
	for i, p := range peers {
		seenIDs := make(map[string]bool)
		for j := 0; j < requests; j++ {
			reply := p.read(t)
			var result struct {
				Owner string `json:"owner"`
			}
			if err := json.Unmarshal(reply["result"], &result); err != nil {
				t.Fatalf("client %d unexpected reply %v: %v", i, reply, err)
			}
			id := string(reply.ID())
			if seenIDs[id] || result.Owner != fmt.Sprintf("harness-%d-request-%s", i, id) {
				t.Fatalf("client %d lost or received another harness's result: %v", i, reply)
			}
			seenIDs[id] = true
		}
	}
	b.mu.Lock()
	remaining := len(b.pending)
	b.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d requests remained pending after all replies", remaining)
	}
}
