// Package uploadv1 will contain generated bindings after scaffold generation.
package uploadv1

// ARCHITECTURE SKETCH: generator checks exact tool versions; bindings are not
// needed by the independent domain signatures in this candidate. Install the
// two Go generators at these versions into a private GOBIN before generation.
//go:generate sh -ec "test \"`protoc --version`\" = \"libprotoc 35.1\"; test \"`protoc-gen-go --version`\" = \"protoc-gen-go v1.36.12\"; test \"`protoc-gen-go-grpc --version`\" = \"protoc-gen-go-grpc 1.6.0\"; protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative uploader.proto"
