package check

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aguadoenzo/mirador/internal/registry"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
)

const defaultInterval = time.Minute * 30

// Check has a responsibility to detect out-of-date images and update them
type Check interface {
	Run(ctx context.Context) error
}

type check struct {
	Registry      registry.Registry
	Docker        *docker.Client
	ContainerChan chan<- *types.ContainerJSON
}

func (c *check) check(ctx context.Context) error {
	log.Println("Check: Checking for image updates...")

	if err := c.checkImageUpdates(ctx); err != nil {
		return fmt.Errorf("failed to check image updates containers: %v", err)
	}

	return nil
}

// Run starts the main loop of the manager
func (c *check) Run(ctx context.Context) error {
	if err := c.check(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(defaultInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.check(ctx); err != nil {
				log.Printf("Check: Failed to check for updates: %v", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// pullImage attempts to pull the given imageRef (name + tag) from the registry.
// If successful, it returns the ID of the pulled image
func (c *check) pullImage(ctx context.Context, imageRef string) (string, error) {
	log.Printf("Pull: Image pull requested for %s", imageRef)
	reader, err := c.Docker.ImagePull(ctx, imageRef, types.ImagePullOptions{})
	if err != nil {
		return "", err
	}
	if err := reader.Close(); err != nil {
		return "", err
	}

	// The Docker SDK does not return the newly pulled image ID, so we have to find it ourselves
	// by taking the image with the appropriate reference that is created last
	args := filters.NewArgs(filters.Arg("reference", imageRef))
	list, err := c.Docker.ImageList(ctx, types.ImageListOptions{Filters: args})

	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", errors.New("empty list after pull")
	}

	latestImage := list[0]
	for _, image := range list {
		if image.Created > latestImage.Created {
			latestImage = image
		}
	}

	return latestImage.ID, nil
}

// refForUntaggedImage takes in an untagged image and looks for the latest version of this image with
// tags.
// TODO: handle diferent tags
// TODO: handle multiple tags per image
func (c *check) refForUntaggedImage(images []types.ImageSummary, untaggedImage *types.ImageSummary) (string, error) {
	if len(untaggedImage.RepoDigests) == 0 {
		return "", errors.New("untagged image RepoDigests is empty, tag is unrecoverable")
	}

	untaggedImageName := strings.Split(untaggedImage.RepoDigests[0], "@")[0]
	for _, image := range images {
		if len(image.RepoTags) == 0 {
			continue
		}

		imageName := strings.Split(image.RepoTags[0], ":")[0]
		if imageName == untaggedImageName {
			return image.RepoTags[0], nil
		}
	}

	return "", errors.New("not found")
}

// TODO: handle when an image has multiple tags
// hasLocalUpdate returns true if the given currentImage has a more up-to-date version locally
func (c *check) hasLocalUpdate(currentImage *types.ImageSummary, images []types.ImageSummary) (bool, error) {
	if len(currentImage.RepoDigests) == 0 {
		return false, errors.New("no repo digests on current image")
	}

	currentImageName := strings.Split(currentImage.RepoDigests[0], "@")[0]

	for _, image := range images {
		if len(image.RepoDigests) == 0 {
			continue
		}

		for _, digest := range image.RepoDigests {
			name := strings.Split(digest, "@")[0]
			if currentImageName == name && image.Created > currentImage.Created {
				return true, nil
			}
		}
	}

	return false, nil
}

// hasRemoteUpdate takes in an image reference (name + tag) and gets the latest corresponding digest from the registry.
// It then checks that remote digest against the local images to see if it already exists. If it does, hasRemoteUpdate
// returns true, otherwise false
func (c *check) hasRemoteUpdate(ctx context.Context, localImages []types.ImageSummary, imageRef string) (bool, error) {
	ref := strings.Split(imageRef, ":")
	if len(ref) != 2 {
		return false, errors.New("malformed image reference")
	}

	name := ref[0]
	tag := ref[1]

	// DockerHub has a "hidden" namespace for images without an explicit namespace (e.g. "ubuntu")
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}

	// Get digest from latest image version upstream
	remoteDigest, err := c.Registry.Digest(ctx, name, tag, "")
	if err != nil {
		return false, fmt.Errorf("failed to get remote digest: %v", err)
	}

	// Check if we have a matching digest locally
	for _, image := range localImages {
		for _, repoTag := range image.RepoTags {
			if repoTag == imageRef {
				for _, repoDigest := range image.RepoDigests {
					if strings.Split(repoDigest, ":")[1] == remoteDigest {
						return false, nil
					}
				}
			}
		}
	}

	return true, nil
}

func (c *check) queueContainerForUpdate(container *types.ContainerJSON) {
	// TODO: use a better domain
	autoUpdate, ok := container.Config.Labels["me.aguado.mirador.automatic-update"]
	// FIXME: remove this 'true'
	if ok && autoUpdate == "true" {
		log.Printf("Check: Queuing container %s for update", container.ID[0:10])
		c.ContainerChan <- container
	} else {
		log.Printf("Check: Skipping container %s for automatic update", container.ID[0:10])
	}
}

func (c *check) checkImageUpdates(ctx context.Context) error {
	images, err := c.Docker.ImageList(ctx, types.ImageListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list images: %v", err)
	}

	containers, err := c.Docker.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %v", err)
	}

	for _, cnt := range containers {
		container, err := c.Docker.ContainerInspect(ctx, cnt.ID)
		if err != nil {
			log.Printf("Check: Failed to inspect container %s: %v", cnt.ID, err)
			continue
		}

		var image *types.ImageSummary
		for _, i := range images {
			if i.ID == container.Image {
				image = &i
				break
			}
		}

		imageRef := ""
		if len(image.RepoTags) > 0 {
			// TODO: handle multiple tags
			imageRef = image.RepoTags[0]
		} else {
			// Image is untagged, so we have to look for a tagged image with the same name
			imageRef, err = c.refForUntaggedImage(images, image)
			if err != nil {
				log.Printf("Check: Failed to get tag for untagged image %s: %v", container.Config.Image, err)
			}
		}

		hasRemoteUpdate, err := c.hasRemoteUpdate(ctx, images, imageRef)
		if err != nil {
			log.Printf("Check: Failed to search for remote update for %s: %v", imageRef, err)
			continue
		}
		if hasRemoteUpdate {
			_, err = c.pullImage(ctx, imageRef)
			if err != nil {
				log.Printf("Check: Failed to pull image: %v", err)
				continue
			}
			c.queueContainerForUpdate(&container)
		} else {
			hasLocalUpdate, err := c.hasLocalUpdate(image, images)
			if err != nil {
				log.Printf("Check: Failed to check for local update: %v", err)
				continue
			}

			if hasLocalUpdate {
				c.queueContainerForUpdate(&container)
			}
		}
	}

	return nil
}

func New(registry registry.Registry, dockerClient *docker.Client, containerChan chan<- *types.ContainerJSON) Check {
	return &check{
		Registry:      registry,
		Docker:        dockerClient,
		ContainerChan: containerChan,
	}
}
