//go:build server

// Package upload terminates the gRPC protocol; storage knows no protobufs.
package upload

// ARCHITECTURE SKETCH: generated bindings are intentionally not required to
// compile these contracts. Scaffold generation precedes implementation.
import (
	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
	"net/http"
)

// Handler owns grpc.Server, metadata authentication and protobuf translation.
// Mount on /mnemonic.upload.v1.Uploader/ in the existing process's h2c mux.
func Handler(*store.Committer, auth.Tokens) http.Handler {
	// TODO: Tokens.Context then Identity.Can(Push); OpenMachine; send snapshot
	// and Ready; receive bounded append fragments; AcceptUpload then lease.Submit;
	// independent send loop encodes only Durable results after Pending.Wait.
	// Offset/head mismatch terminates RPC FailedPrecondition, never sends a
	// conflict position or runs a 409-style retry. Reconnect obtains a snapshot.
	panic("not implemented")
}
