package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"watchtower-exporter/internal/check"
	"watchtower-exporter/internal/registry"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func main() {
	docker, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Printf("Failed to start docker client: %v", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		cancel()
	}()

	reg := registry.New()
	checkMgr := check.New(reg, docker)

	errChan := make(chan error, 10)
	imagesToUpdate := make(chan *types.ImageSummary, 100)

	log.Println("Main: Starting managers")

	go func() {
		if err := checkMgr.Run(ctx, imagesToUpdate); err != nil {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		log.Println(err)
		os.Exit(1)
	case <-ctx.Done():
		log.Println("Main: Interrupt signal received, stopping gracefully")
		break
	}
}
