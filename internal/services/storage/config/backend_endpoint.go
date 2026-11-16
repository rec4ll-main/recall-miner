package config

import (
	"net/url"
	"strings"
)

// BackendEndpoint returns the configured vector backend's address for health and
// registration metadata, with any credentials removed. Operators sometimes put a
// user name, password or token in QDRANT_URL or MILVUS_URI; none of that may leave
// the process. Only scheme, host and port (and path) are kept. A value that cannot be
// parsed as an absolute URL is withheld rather than echoed.
func (c Config) BackendEndpoint() string {
	switch strings.ToLower(c.DBEngine) {
	case "pgsql", "postgres", "postgresql", "pgvector":
		return "pgsql"
	case "qdrant":
		return redactEndpoint(c.QdrantURL)
	case "milvus":
		return redactEndpoint(c.MilvusURI)
	default:
		return ""
	}
}

func redactEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "redacted"
	}
	clean := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return clean.String()
}
