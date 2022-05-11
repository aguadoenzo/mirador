package registry

// TokenResponse holds the response when requesting an authentication token for the registry API
type TokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"`
	IssuedAt  string `json:"issued_at"`
}
