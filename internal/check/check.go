package check

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"watchtower-exporter/internal/registry"

	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

const defaultInterval = time.Minute * 30

// Check has a responsibility to detect out-of-date images
type Check interface {
	Run(ctx context.Context, imageChan chan<- *types.ImageInspect) error
}

type check struct {
	Registry registry.Registry
	Docker   *docker.Client

	// running containers
	containers map[string]*types.Container
}

func (c *check) check(ctx context.Context, imageChan chan<- *types.ImageInspect) error {
	log.Println("Check: Checking for image updates...")

	if err := c.refreshContainers(); err != nil {
		return fmt.Errorf("failed to refresh containers: %v", err)
	}

	if err := c.checkImageUpdates(ctx, imageChan); err != nil {
		return fmt.Errorf("failed to check image updates containers: %v", err)
	}

	return nil
}

func (c *check) Run(ctx context.Context, imageChan chan<- *types.ImageInspect) error {
	if err := c.check(ctx, imageChan); err != nil {
		return err
	}

	ticker := time.NewTicker(defaultInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.check(ctx, imageChan); err != nil {
				log.Printf("Check: Failed to check for updates: %v", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *check) refreshContainers() error {
	containers, err := c.Docker.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %v", err)
	}

	for _, container := range containers {
		// Avoid duplicates, so we don't upgrade an image more than once per run
		c.containers[container.Image] = &container
	}
	return nil
}

func (c *check) checkImageUpdates(ctx context.Context, imageChan chan<- *types.ImageInspect) error {
	images, err := c.Docker.ImageList(ctx, types.ImageListOptions{All: false})
	if err != nil {
		return fmt.Errorf("failed to list images: %v", err)
	}

	for _, container := range c.containers {
		// Find the image relating to the container
		var image types.ImageSummary
		for _, i := range images {
			if len(i.RepoTags) == 0 {
				// Ignore untagged images
				continue
			}
			if i.ID == container.ImageID {
				image = i
			}
		}

		// If no image is found, it's likely that the image is updated (and thus the previous one
		// untagged), but the container hasn't been updated and is still pointing to the old image
		if len(image.ID) == 0 {
			log.Printf("Check: Container %s does not have a matching image", container.ID[0:10])
			continue
		}

		imageRef := strings.Split(image.RepoTags[0], ":")
		if len(imageRef) != 2 {
			log.Printf("Check: Invalid image reference: %v", imageRef)
			continue
		}
		name := imageRef[0]
		tag := imageRef[1]

		// DockerHub has a "hidden" namespace for images without an explicit namespace (e.g. "ubuntu")
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}

		// Get digest from latest image version upstream
		remoteDigest, err := c.Registry.Digest(context.TODO(), name, tag, "")
		if err != nil {
			log.Printf("Check: Failed to get remote digest: %v", err)
			continue
		}

		// Get local image digest
		imageInspect, _, err := c.Docker.ImageInspectWithRaw(context.Background(), image.ID)
		if err != nil {
			log.Printf("Check: Failed to get local digest: %v", err)
			continue
		}

		found := false
		for _, localDigest := range imageInspect.RepoDigests {
			localDigest = strings.Split(localDigest, ":")[1]
			if localDigest == remoteDigest {
				found = true
				break
			}
		}

		if !found {
			log.Printf("Check: Queuing image %s:%s for upgrade", name, tag)
			imageChan <- &imageInspect
		}
	}

	return nil
}

func New(registry registry.Registry, dockerClient *docker.Client) Check {
	return &check{
		Registry:   registry,
		Docker:     dockerClient,
		containers: make(map[string]*types.Container),
	}
}
