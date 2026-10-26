//go:build server

// Package upload terminates the gRPC protocol; storage knows no protobufs.
package upload

// ARCHITECTURE SKETCH: generated bindings are intentionally not required to
// compile these contracts. Scaffold generation precedes implementation.
import (
	"net/http"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

// The receive state is a closed protocol phase. A ready stream can begin a
// body; only receiving can consume fragments/End. Snapshotting accepts no body.
type receivePhase interface{ receivePhase() }
type snapshotting struct{ lease *store.MachineLease }
type ready struct {
	lease        *store.MachineLease
	nextSequence uint64
}
type receiving struct {
	lease    *store.MachineLease
	sequence uint64
	input    store.UploadInput
	received int64
}

func (*snapshotting) receivePhase() { panic("not implemented") }
func (*ready) receivePhase()        { panic("not implemented") }
func (*receiving) receivePhase()    { panic("not implemented") }

// ackSender is filled with the generated stream in the scaffold. Its method
// cannot accept a provisional SourceState, UploadInput, or Pending value.
type ackSender struct{}

func (*ackSender) send(sequence uint64, durable store.Durable) error {
	// TODO: reject nil; encode only the state of the committer-produced receipt;
	// send through the generated stream. Never hold a store lock across Send.
	panic("not implemented")
}

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
