package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"watchtower-exporter/registry"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func main() {
	docker, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Printf("Failed to start docker client: %v", err)
		os.Exit(1)
	}

	containers, err := docker.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		panic(err)
	}
	for _, container := range containers {
		fmt.Printf("%s %s %s\n", container.ID[:10], container.Image, container.ImageID)
	}

	//imagesToCheck := []string{}
	images, err := docker.ImageList(context.Background(), types.ImageListOptions{All: false})
	if err != nil {
		log.Printf("Failed to list images: %v", err)
		os.Exit(1)
	}

	for _, image := range images {
		log.Printf("image %s %d", image.ID, image.Containers)
	}

	reg := registry.New()
	digest, err := reg.Digest(context.TODO(), "library/ubuntu", "latest", "")
	if err != nil {
		log.Printf("Failed to get digest: %v", err)
		os.Exit(1)
	}

	log.Printf("digest = %s", digest)
}
