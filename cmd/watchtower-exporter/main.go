package main

import (
	"context"
	"log"
	"os"
	"strings"

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

	containers, err := docker.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		log.Printf("Failed to list containers: %v", err)
		os.Exit(1)
	}

	images, err := docker.ImageList(context.Background(), types.ImageListOptions{All: false})
	if err != nil {
		log.Printf("Failed to list images: %v", err)
		os.Exit(1)
	}

	// Only check for images that have existing containers
	// TODO: also need to check for running containers
	imagesToCheck := []types.ImageSummary{}
	for _, container := range containers {
		for _, image := range images {
			if container.ImageID == image.ID {
				imagesToCheck = append(imagesToCheck, image)
			}
		}
	}

	reg := registry.New()

	for _, image := range imagesToCheck {
		imageRef := strings.Split(image.RepoTags[0], ":")
		if len(imageRef) != 2 {
			log.Println("Invalid image reference")
			os.Exit(1)
		}

		name := imageRef[0]
		tag := imageRef[1]

		if !strings.Contains(name, "/") {
			name = "library/" + name
		}

		latestDigest, err := reg.Digest(context.TODO(), name, tag, "")
		if err != nil {
			log.Printf("Failed to get digest: %v", err)
			os.Exit(1)
		}

		imageInfo, _, err := docker.ImageInspectWithRaw(context.Background(), image.ID)
		if err != nil {
			log.Printf("Failed to inspect image %s: %v", image.RepoTags[0], err)
			os.Exit(1)
		}

		found := false
		for _, localDigest := range imageInfo.RepoDigests {
			localDigest = strings.Split(localDigest, ":")[1]
			if localDigest == latestDigest {
				found = true
				break
			}
		}

		if found {
			log.Println("Image", name, "tagged", tag, "is up to date")
		} else {
			log.Println("image requires update")
		}
	}
}
