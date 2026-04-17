// Package main provides a standalone entry point for the types-server.
package main

import (
	"log"

	"activity-reward-extension/internal/config"
	"activity-reward-extension/internal/typesserver"
	"activity-reward-extension/pkg/decoder"
	"activity-reward-extension/pkg/types"
)

func main() {
	registry := decoder.NewRegistry()
	types.RegisterDecoders(registry)

	s := typesserver.New(registry)
	log.Fatal(s.ListenAndServe(config.TypesServerPort))
}
