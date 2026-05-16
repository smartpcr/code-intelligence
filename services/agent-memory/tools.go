//go:build tools
// +build tools

// Package tools pins the protoc plugin binaries we generate
// `proto/agent/*.pb.go` with so a reproducible toolchain
// regenerates the same bytes.
//
// Regenerate with:
//
//	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
//	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
//	protoc \
//	  --go_out=. --go_opt=module=github.com/smartpcr/code-intelligence/services/agent-memory \
//	  --go-grpc_out=. --go-grpc_opt=module=github.com/smartpcr/code-intelligence/services/agent-memory \
//	  proto/agent.proto
//
// The build tag `tools` keeps these imports out of the
// production binary; `go mod tidy` still resolves the
// versions so `go install` above pulls the pinned releases.
package tools

import (
	_ "google.golang.org/grpc/cmd/protoc-gen-go-grpc"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)
