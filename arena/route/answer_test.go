package route

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/candyflipline/mnemonic/arena/keeper"
)

type garbled struct{}

func (garbled) ID() string { return "r1" }

func (garbled) Call(context.Context, CallRequest) (CallResponse, error) {
	return CallResponse{Phase: json.RawMessage(`{"not":"a df list"}`)}, nil
}

func TestUndecodablePhaseAnswerIsInfra(t *testing.T) {
	v := View{Tenant: "acme", Parts: []GrantedPart{{PartRef: keeper.PartRef{ID: "p1"}, URL: "http://unused"}}}
	_, _, err := Do(context.Background(), v, []Reader{garbled{}}, Partial, "search_sessions", json.RawMessage(`{"query":"hello world"}`))
	var infra InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("Do error = %v, want an InfraError", err)
	}
}
