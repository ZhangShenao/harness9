package provider

import (
	"context"
	"testing"

	"github.com/harness9/internal/schema"
)

func TestSendStreamChunk_DeliversToChannel(t *testing.T) {
	ch := make(chan schema.StreamChunk, 1)
	chunk := schema.StreamChunk{Type: schema.StreamChunkTextDelta, Delta: "hello"}

	sent := sendStreamChunk(context.Background(), ch, chunk)
	if !sent {
		t.Fatal("expected sendStreamChunk to return true")
	}
	select {
	case got := <-ch:
		if got.Delta != "hello" {
			t.Errorf("expected delta 'hello', got %q", got.Delta)
		}
	default:
		t.Fatal("channel should have received the chunk")
	}
}

func TestSendStreamChunk_ContextCancelled_ReturnsFalse(t *testing.T) {
	ch := make(chan schema.StreamChunk) // unbuffered — nobody reading
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	chunk := schema.StreamChunk{Type: schema.StreamChunkTextDelta, Delta: "hello"}
	sent := sendStreamChunk(ctx, ch, chunk)
	if sent {
		t.Fatal("expected sendStreamChunk to return false when context is already cancelled")
	}
}

func TestSendStreamChunk_BlocksUntilRead(t *testing.T) {
	ch := make(chan schema.StreamChunk) // unbuffered
	chunk := schema.StreamChunk{Type: schema.StreamChunkDone}

	done := make(chan bool, 1)
	go func() {
		sent := sendStreamChunk(context.Background(), ch, chunk)
		done <- sent
	}()

	<-ch // unblock the sender

	if !<-done {
		t.Fatal("expected sendStreamChunk to return true after successful send")
	}
}
