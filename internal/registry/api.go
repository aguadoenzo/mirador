package registry

// TokenResponse holds the response when requesting an authentication token for the registry API
type TokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
	IssuedAt  string `json:"issued_at"`
}

// Manifest holds information about an image, such as layers, size and digest
type Manifest struct {
	// Digest is a sha256 hash of the image
	Digest string `json:"digest"`

	// MediaType identifies the version and type of manifest
	MediaType string `json:"mediaType"`

	// Size in bytes of the manifest
	Size int `json:"size"`

	// Platform supported by the image. Contains OS and architecture information
	Platform struct {
		// Architecture supported by the image
		Architecture string `json:"architecture"`

		// OS defines the operating system this image can run on
		OS string `json:"os"`
	}
}

// ManifestList is a fat manifest holding references to other platform-specific manifests
type ManifestList struct {
	// MediaType identifies the version and type of manifest
	MediaType string `json:"mediaType"`

	// Manifests holds all image manifests for all platforms
	Manifests []Manifest `json:"manifests"`

	// Version of the manifest
	SchemaVersion int `json:"schemaVersion"`
}
