package update

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"log"
	"strings"
)

type Update interface {
	Run(ctx context.Context) error
}

const miradorBackupNameSuffix = "-mirador-backup"

type update struct {
	Docker             *docker.Client
	ContainersToUpdate <-chan *types.ContainerJSON
}

func (u *update) Run(ctx context.Context) error {
	for {
		select {
		case container := <-u.ContainersToUpdate:
			go func(ctx context.Context, cnt *types.ContainerJSON) {
				if err := u.updateContainer(ctx, container); err != nil {
					log.Printf("Update: Failed to update container: %v", err)
				}
			}(ctx, container)
		case <-ctx.Done():
			return nil
		}
	}
}

// SliceSubtract subtracts the content of slice a2 from slice a1
// https://github.com/containrrr/watchtower/blob/56368a72074167642ac68211ca2e35de97e00f66/internal/util/util.go#L19
func SliceSubtract(a1, a2 []string) []string {
	a := []string{}

	for _, e1 := range a1 {
		found := false

		for _, e2 := range a2 {
			if e1 == e2 {
				found = true
				break
			}
		}

		if !found {
			a = append(a, e1)
		}
	}

	return a
}

// StringMapSubtract subtracts the content of structmap m2 from structmap m1
func StringMapSubtract(m1, m2 map[string]string) map[string]string {
	m := map[string]string{}

	for k1, v1 := range m1 {
		if v2, ok := m2[k1]; ok {
			if v2 != v1 {
				m[k1] = v1
			}
		} else {
			m[k1] = v1
		}
	}

	return m
}

// StructMapSubtract subtracts the content of structmap m2 from structmap m1
func StructMapSubtract(m1, m2 map[string]struct{}) map[string]struct{} {
	m := map[string]struct{}{}

	for k1, v1 := range m1 {
		if _, ok := m2[k1]; !ok {
			m[k1] = v1
		}
	}

	return m
}

// SliceEqual compares two slices and checks whether they have equal content
func SliceEqual(s1, s2 []string) bool {
	if len(s1) != len(s2) {
		return false
	}

	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}

	return true
}

func (u *update) latestVersionOfImage(ctx context.Context, oldImageID string) (*types.ImageSummary, error) {
	oldImage, _, err := u.Docker.ImageInspectWithRaw(ctx, oldImageID)
	if err != nil {
		return nil, err
	}

	if len(oldImage.RepoDigests) == 0 {
		return nil, errors.New("untagged image RepoDigests is empty, name is unrecoverable")
	}
	imageName := strings.Split(oldImage.RepoDigests[0], "@")[0]

	// TODO: handle multiple tags per image
	args := filters.NewArgs(filters.Arg("reference", imageName))
	list, err := u.Docker.ImageList(ctx, types.ImageListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no up to date image with given tag %s", oldImage.RepoDigests[0])
	}

	// And pick the latest
	latest := list[0]
	for _, candidate := range list {
		if candidate.Created > latest.Created {
			latest = candidate
		}
	}

	return &latest, nil
}

// Taken from Watchtower https://github.com/containrrr/watchtower/blob/56368a72074167642ac68211ca2e35de97e00f66/pkg/container/container.go#L230
// Ideally, we'd just be able to take the ContainerConfig from the old container
// and use it as the starting point for creating the new container; however,
// the ContainerConfig that comes back from the Inspect call merges the default
// configuration (the stuff specified in the metadata for the image itself)
// with the overridden configuration (the stuff that you might specify as part
// of the "docker run"). In order to avoid unintentionally overriding the
// defaults in the new image we need to separate the override options from the
// default options. To do this we have to compare the ContainerConfig for the
// running container with the ContainerConfig from the image that container was
// started from. This function returns a ContainerConfig which contains just
// the options overridden at runtime.
func (u *update) updateContainer(ctx context.Context, container *types.ContainerJSON) error {
	newImage, err := u.latestVersionOfImage(ctx, container.Image)
	if err != nil {
		return fmt.Errorf("failed to get latest image version: %v", err)
	}

	oldImage, _, err := u.Docker.ImageInspectWithRaw(ctx, container.Image)
	if err != nil {
		return fmt.Errorf("failed to inspect image: %v", err)
	}

	log.Printf("Update: Start updating container %s (%s) from image %s to new image %s (%s)", container.Name, container.ID[:10], container.Image[:20], newImage.ID[:20], newImage.RepoTags)

	cntConfig := container.Config
	// We use the reference rather than the ID to have a human-readable name in the "IMAGE" column of `docker ps`
	cntConfig.Image = newImage.RepoTags[0]

	if cntConfig.WorkingDir == oldImage.Config.WorkingDir {
		cntConfig.WorkingDir = ""
	}

	if cntConfig.User == oldImage.Config.User {
		cntConfig.User = ""
	}
	cntConfig.Env = SliceSubtract(cntConfig.Env, oldImage.Config.Env)
	cntConfig.Labels = StringMapSubtract(cntConfig.Labels, oldImage.Config.Labels)
	cntConfig.Volumes = StructMapSubtract(cntConfig.Volumes, oldImage.Config.Volumes)
	if SliceEqual(cntConfig.Entrypoint, oldImage.Config.Entrypoint) {
		cntConfig.Entrypoint = nil
	}

	if SliceEqual(cntConfig.Cmd, oldImage.Config.Cmd) {
		cntConfig.Cmd = nil
	}

	for k := range cntConfig.ExposedPorts {
		if _, ok := oldImage.Config.ExposedPorts[k]; ok {
			delete(cntConfig.ExposedPorts, k)
		}
	}

	hostConfig := container.HostConfig
	if hostConfig.NetworkMode.IsContainer() {
		cntConfig.Hostname = ""
	}
	for p := range hostConfig.PortBindings {
		cntConfig.ExposedPorts[p] = struct{}{}
	}

	var netConfig *network.NetworkingConfig
	for k, v := range container.NetworkSettings.Networks {
		// Docker cannot create a container with multiple networks. We need to add a single
		// one, and attach subsequent networks after creation
		singleEndpoint := make(map[string]*network.EndpointSettings)
		singleEndpoint[k] = v

		netConfig = &network.NetworkingConfig{
			EndpointsConfig: singleEndpoint,
		}
	}
	name := container.Name

	// 1. Stop the existing container
	log.Printf("Update: Stopping container %s", container.ID[:10])
	err = u.Docker.ContainerStop(ctx, container.ID, nil)
	if err != nil {
		return fmt.Errorf("failed to stop container %s: %v", container.ID[0:10], err)
	}

	oldName := container.Name
	// 2. Rename the existing container, so we can create the new one with an identical name
	log.Printf("Update: Renaming container %s to %s", oldName, oldName+miradorBackupNameSuffix)
	err = u.Docker.ContainerRename(ctx, container.ID, oldName+miradorBackupNameSuffix)

	// 3. Create the container with the appropriate configuration and the new image
	log.Printf("Update: Recreating container %s", oldName)
	createdContainer, err := u.Docker.ContainerCreate(ctx, cntConfig, hostConfig, netConfig, nil, name)
	if err != nil {
		return fmt.Errorf("failed to create container: %v", err)
	}

	// 4. Start the new container and make sure everything is fine
	log.Printf("Update: Starting container %s (new ID: %s)", oldName, createdContainer.ID[:10])
	err = u.Docker.ContainerStart(ctx, createdContainer.ID, types.ContainerStartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start container %s: %v", createdContainer.ID[0:10], err)
	}

	// 5. Remove the old container
	log.Printf("Update: Removing old container %s", container.ID[:10])
	err = u.Docker.ContainerRemove(ctx, container.ID, types.ContainerRemoveOptions{
		RemoveVolumes: false,
		RemoveLinks:   false,
		Force:         false,
	})

	log.Printf("Update: Created new container %s (%s) based on image %s", name, createdContainer.ID[0:10], cntConfig.Image)
	return nil
}

func New(client *docker.Client, containersChan <-chan *types.ContainerJSON) Update {
	return &update{
		Docker:             client,
		ContainersToUpdate: containersChan,
	}
}
