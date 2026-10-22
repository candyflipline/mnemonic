package route

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
)

type answers struct{ id string }

func (a answers) ID() string { return a.id }

func (a answers) Call(context.Context, CallRequest) (CallResponse, error) {
	return CallResponse{Phase: json.RawMessage(`{"found":true,"data":"{\"n\":1}","text":"opened"}`)}, nil
}

func TestStalledReaderFailsOver(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) })

	rank := Owner("p1", []string{"a", "b"})
	readers := []Reader{HTTPReader(rank[0], stalled.URL, 200*time.Millisecond), answers{rank[1]}}
	v := View{Tenant: "acme", Parts: []GrantedPart{{PartRef: keeper.PartRef{ID: "p1", Sessions: []string{"s1"}}, URL: "http://unused"}}}

	type result struct {
		data json.RawMessage
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, text, err := Do(context.Background(), v, readers, Partial, "open", json.RawMessage(`{"id":"session:czE"}`))
		done <- result{data, text, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || string(r.data) != `{"n":1}` || r.text != "opened" {
			t.Fatalf("Do = %s %q %v, want the next rank's answer", r.data, r.text, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do still blocked on a stalled reader after 5 s")
	}
}
