package main

import (
	"log"

	"zshell-gateway/internal/gateway"
)

func main() {
	if err := gateway.Run(); err != nil {
		log.Fatal(err)
	}
}
