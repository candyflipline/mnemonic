package pushpb

//go:generate sh -c "export GOBIN=$(mktemp -d) && go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.0 && protoc --plugin=protoc-gen-go=${DOLLAR}GOBIN/protoc-gen-go --plugin=protoc-gen-go-grpc=${DOLLAR}GOBIN/protoc-gen-go-grpc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative push.proto"
