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

	images     []types.ImageSummary
	containers []types.Container
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

// hasLocalUpdate returns true if the given currentImage has a more up-to-date version locally
func (c *check) hasLocalUpdate(container *types.ContainerJSON, currentImage *types.ImageSummary) (bool, error) {
	if len(currentImage.RepoDigests) == 0 {
		return false, errors.New("no repo digests on current image")
	}

	if len(currentImage.RepoTags) > 1 {
		return false, errors.New("image has ambiguous tags")
	}

	ref, err := c.refForImage(container, currentImage)
	if err != nil {
		return false, err
	}

	candidates := []types.ImageSummary{}
	for _, image := range c.images {
		if len(image.RepoTags) == 0 {
			continue
		} else if len(image.RepoTags) > 1 {
			log.Printf("Check: Image %s has ambiguous tags", image.ID[:20])
			continue
		}

		if image.RepoTags[0] == ref {
			candidates = append(candidates, image)
		}
	}

	for _, candidate := range candidates {
		if candidate.Created > currentImage.Created {
			return true, nil
		}
	}
	return false, nil
}

// hasRemoteUpdate takes in an image reference (name + tag) and gets the latest corresponding digest from the registry.
// It then checks that remote digest against the local images to see if it already exists. If it does, hasRemoteUpdate
// returns true, otherwise false
func (c *check) hasRemoteUpdate(ctx context.Context, imageRef string) (bool, error) {
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
	for _, image := range c.images {
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
	if ok && autoUpdate == "true" {
		log.Printf("Check: Queuing container %s for update", container.ID[0:10])
		c.ContainerChan <- container
	} else {
		log.Printf("Check: Skipping container %s for automatic update", container.ID[0:10])
	}
}

// refForImage attempts to figure out the reference (name + tag) of an image.
// There can be four scenarios:
//   1. The container running this image has a "tag" label:
//      We return the user specified tag in the label
//
//   2. The image has a single tag:
//      We return that tag
//
//   3. The image does not have any tag (it was updated and untagged):
//      We get its name and get the latest image with the same name and take its tag
//      NOTE: an error is returned if multiple images with the same name but different
//      tags exist (ambiguous tags)
//
//   4. The image has multiple tags (it was manually tagged):
//      We can't determine which tag should be taken into account, an error is returned
func (c *check) refForImage(container *types.ContainerJSON, image *types.ImageSummary) (string, error) {
	if tag, ok := container.Config.Labels["me.aguado.mirador.reference"]; ok {
		return tag, nil
	}

	switch len(image.RepoTags) {
	case 0:
		// The repo digest usually contains the name
		if len(image.RepoDigests) == 0 {
			return "", errors.New("image RepoDigests is empty, tag is unrecoverable")
		} else if len(image.RepoDigests) > 1 {
			return "", errors.New("image has multiple repo digests, name is ambiguous")
		}

		name := strings.Split(image.RepoDigests[0], "@")[0]

		// Gather all images with the same name
		candidates := []types.ImageSummary{}
		for _, candidate := range c.images {
			if len(candidate.RepoTags) == 0 {
				continue
			} else if len(candidate.RepoTags) > 1 {
				log.Printf("Check: Image %s has multiple repo tags, name is ambiguous", candidate.ID[:20])
				continue
			}

			if strings.Split(candidate.RepoTags[0], ":")[0] == name {
				candidates = append(candidates, candidate)
			}
		}

		if len(candidates) == 0 {
			return "", errors.New("not found")
		}

		// Make sure there is only a single tag
		for i := 0; i < len(candidates); i++ {
			if candidates[i].RepoTags[0] != candidates[i+1].RepoTags[0] {
				return "", errors.New("multiple tags for same image name, tags are ambiguous")
			}
		}

		// Take the latest image
		latest := candidates[0]
		for _, candidate := range candidates {
			if candidate.Created > latest.Created {
				latest = candidate
			}
		}

		return latest.RepoTags[0], nil

	case 1:
		return image.RepoTags[0], nil

	default:
		return "", errors.New("ambiguous tags")
	}
}

func (c *check) checkImageUpdates(ctx context.Context) error {
	var err error
	c.images, err = c.Docker.ImageList(ctx, types.ImageListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list images: %v", err)
	}

	c.containers, err = c.Docker.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %v", err)
	}

	for _, cnt := range c.containers {
		container, err := c.Docker.ContainerInspect(ctx, cnt.ID)
		if err != nil {
			log.Printf("Check: Failed to inspect container %s: %v", cnt.ID, err)
			continue
		}

		var image *types.ImageSummary
		for _, i := range c.images {
			if i.ID == container.Image {
				image = &i
				break
			}
		}

		imageRef, err := c.refForImage(&container, image)
		if err != nil {
			log.Printf("Check: Failed to get reference of image %s: %v", image.ID[:20], err)
			continue
		}

		hasRemoteUpdate, err := c.hasRemoteUpdate(ctx, imageRef)
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
			// Refresh images
			c.images, err = c.Docker.ImageList(ctx, types.ImageListOptions{})
			if err != nil {
				return fmt.Errorf("failed to list images: %v", err)
			}

			c.queueContainerForUpdate(&container)
		} else {
			hasLocalUpdate, err := c.hasLocalUpdate(&container, image)
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
