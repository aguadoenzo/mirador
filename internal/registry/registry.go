package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type Registry interface {
	// Digest returns the digest associated with a docker image and tag (aka the image hash)
	Digest(ctx context.Context, image, tag, authToken string) (string, error)
}

// ContentDigestHeader is the key for the key-value pair containing the digest header
const ContentDigestHeader = "Docker-Content-Digest"
const dockerHubUrl = "https://index.docker.io/v2"

type registryImpl struct {
	client *http.Client
}

// authenticate returns an authentication token for a given service and scope
func (r *registryImpl) authenticate(ctx context.Context, realm string, service string, scope string) (string, error) {
	uri := fmt.Sprintf("%s?service=%s&scope=%s", realm, service, scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	rawResp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}

	if rawResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("expected 200, got %d", rawResp.StatusCode)
	}

	resp := TokenResponse{}
	if err := json.NewDecoder(rawResp.Body).Decode(&resp); err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	return resp.Token, nil
}

// Digest returns the digest associated with a docker image and a tag
// If authToken is empty and the request fails because of a 401 error, an authentication attempt will be performed
func (r *registryImpl) Digest(ctx context.Context, image, tag, authToken string) (string, error) {
	uri := fmt.Sprintf("%s/%s/manifests/%s", dockerHubUrl, image, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, uri, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	// Running "docker images --digests" return the fat manifest digest of an image
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.list.v2+json")

	if len(authToken) > 0 {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authToken))
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}

	if resp.StatusCode == http.StatusUnauthorized && len(authToken) == 0 {
		authHeader := resp.Header.Get("www-authenticate")
		if len(authHeader) == 0 {
			return "", errors.New("request is unauthorized with no www-authenticate header returned")
		}

		authHeader = strings.ReplaceAll(authHeader, "Bearer ", "")

		var url, service, scope string
		fields := strings.Split(authHeader, ",")
		for _, field := range fields {
			kv := strings.Split(field, "=")
			kv[1] = strings.ReplaceAll(kv[1], "\"", "")
			switch kv[0] {
			case "realm":
				url = kv[1]
				break
			case "service":
				service = kv[1]
				break
			case "scope":
				scope = kv[1]
				break
			default:
				log.Printf("Registry: Unknown field in www-authenticate header: %s", field)
			}
		}

		token, err := r.authenticate(ctx, url, service, scope)
		if err != nil {
			return "", fmt.Errorf("failed to get authentication token %v", err)
		}

		log.Println("Registry: Initial request failed, attempting authentication")
		return r.Digest(ctx, image, tag, token)
	} else if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request failed with code %d", resp.StatusCode)
	}

	digest := resp.Header.Get(ContentDigestHeader)
	if len(digest) == 0 {
		return "", errors.New("empty digest header")
	}

	return strings.TrimPrefix(digest, "sha256:"), nil
}

func New() Registry {
	reg := &registryImpl{}
	reg.client = &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout: time.Second * 10,
		},
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       time.Second * 10,
	}

	return reg
}
