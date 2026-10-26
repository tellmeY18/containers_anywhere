package main

import (
	"log"
	"os"

	"github.com/tellmeY18/containers_anywhere/internal/guest"
)

func main() {
	if err := guest.Main(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
