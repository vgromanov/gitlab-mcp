//go:build !unix

package tlsx

// Unsupported platforms refuse explicit CA file loads (default-off closed stub).
func readBoundedCA(string) ([]byte, error) { return nil, ErrMalformedCA }
