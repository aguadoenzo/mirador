package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	"github.com/aguadoenzo/mirador/internal/check"
	"github.com/aguadoenzo/mirador/internal/registry"
	"github.com/aguadoenzo/mirador/internal/update"

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

	containersToUpdate := make(chan *types.ContainerJSON, 100)
	checkMgr := check.New(reg, docker, containersToUpdate)
	updateMgr := update.New(docker, containersToUpdate)

	errChan := make(chan error, 10)

	log.Println("Main: Starting managers")

	go func() {
		if err := checkMgr.Run(ctx); err != nil {
			errChan <- err
		}
	}()

	go func() {
		if err := updateMgr.Run(ctx); err != nil {
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
