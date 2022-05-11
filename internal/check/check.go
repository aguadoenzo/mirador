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
	Run(ctx context.Context, imageChan chan<- *types.ImageSummary) error
}

type check struct {
	Registry registry.Registry
	Docker   *docker.Client

	// running containers
	containers map[string]*types.Container
}

func (c *check) check(ctx context.Context, imageChan chan<- *types.ImageSummary) error {
	log.Println("Check: Checking for image updates...")

	if err := c.refreshContainers(); err != nil {
		return fmt.Errorf("failed to refresh containers: %v", err)
	}

	if err := c.checkImageUpdates(ctx, imageChan); err != nil {
		return fmt.Errorf("failed to check image updates containers: %v", err)
	}

	return nil
}

func (c *check) Run(ctx context.Context, imageChan chan<- *types.ImageSummary) error {
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

func (c *check) checkImageUpdates(ctx context.Context, imageChan chan<- *types.ImageSummary) error {
	images, err := c.Docker.ImageList(ctx, types.ImageListOptions{All: false})
	if err != nil {
		return fmt.Errorf("failed to list images: %v", err)
	}

	for _, container := range c.containers {
		log.Println("checking container", container.Image)

		// Find the image relating to the container
		var image types.ImageSummary
		for _, i := range images {
			if i.ID == container.ImageID {
				image = i
			}
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
		imageInfo, _, err := c.Docker.ImageInspectWithRaw(context.Background(), image.ID)
		if err != nil {
			log.Printf("Check: Failed to get local digest: %v", err)
			continue
		}

		found := false
		for _, localDigest := range imageInfo.RepoDigests {
			localDigest = strings.Split(localDigest, ":")[1]
			log.Printf("local digest %s ||| remote %s", localDigest, remoteDigest)
			if localDigest == remoteDigest {
				found = true
				break
			}
		}

		if !found {
			log.Printf("Check: Queuing image %s:%s for upgrade", name, tag)
			imageChan <- &image
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
