// Package proto keeps the source protobuf definitions and their generation command.
package proto

//go:generate protoc --proto_path=. --go_out=.. --go_opt=module=github.com/SarnautCore/server sarnaut/v1/chat.proto sarnaut/v1/common.proto sarnaut/v1/envelope.proto sarnaut/v1/handshake.proto sarnaut/v1/hud.proto sarnaut/v1/movement.proto sarnaut/v1/replication.proto
//go:generate protoc --proto_path=. --go_out=.. --go_opt=module=github.com/SarnautCore/server sarnaut/content/v1/content.proto
//go:generate protoc --proto_path=. --go_out=.. --go_opt=module=github.com/SarnautCore/server sarnaut/auth/v1/auth.proto
