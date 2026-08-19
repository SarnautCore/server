// Package proto keeps the source protobuf definitions and their generation command.
package proto

//go:generate protoc --proto_path=. --go_out=.. --go_opt=module=github.com/SarnautCore/server sarnaut/v1/common.proto sarnaut/v1/handshake.proto
