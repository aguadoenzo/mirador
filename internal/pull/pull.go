package pull

import (
	"context"
	"log"

	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

type Pull interface {
	Run(ctx context.Context, images <-chan *types.ImageInspect) error
}

type pull struct {
	Docker *docker.Client
}

func (p *pull) Run(ctx context.Context, images <-chan *types.ImageInspect) error {
	for {
		select {
		case image := <-images:
			go func(ctx context.Context, image *types.ImageInspect) {
				if err := p.pullImage(ctx, image); err != nil {
					log.Printf("Pull: Failed to pull image: %v", err)
				}
			}(ctx, image)
		case <-ctx.Done():
			return nil
		}
	}
}

func (p *pull) pullImage(ctx context.Context, image *types.ImageInspect) error {
	log.Printf("Pull: Image pull requested for %s", image.RepoTags[0])
	// TODO: might need to check for all the repo tags
	reader, err := p.Docker.ImagePull(ctx, image.RepoTags[0], types.ImagePullOptions{
		All:          false,
		RegistryAuth: "",
		Platform:     image.Architecture,
	})

	if err != nil {
		return err
	}

	return reader.Close()
}

func New(docker *docker.Client) Pull {
	return &pull{
		Docker: docker,
	}
}
