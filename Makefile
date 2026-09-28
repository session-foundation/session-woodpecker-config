.PHONY: session-woodpecker-config
session-woodpecker-config:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/session-woodpecker-config
