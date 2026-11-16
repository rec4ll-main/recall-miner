package config

import (
	"strings"
	"testing"
)

func TestBackendEndpointStripsCredentials(t *testing.T) {
	const secret = "s3cr3t-fixture"
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"qdrant default", Config{DBEngine: "qdrant", QdrantURL: "http://qdrant:6333"}, "http://qdrant:6333"},
		{"qdrant userinfo", Config{DBEngine: "qdrant", QdrantURL: "https://user:" + secret + "@db.example.invalid:6333"}, "https://db.example.invalid:6333"},
		{"qdrant query key", Config{DBEngine: "qdrant", QdrantURL: "https://db.example.invalid:6333/?api-key=" + secret}, "https://db.example.invalid:6333/"},
		{"milvus default", Config{DBEngine: "milvus", MilvusURI: "http://milvus:19530"}, "http://milvus:19530"},
		{"milvus userinfo", Config{DBEngine: "MILVUS", MilvusURI: "https://root:" + secret + "@milvus.example.invalid:19530"}, "https://milvus.example.invalid:19530"},
		{"milvus token query and fragment", Config{DBEngine: "milvus", MilvusURI: "http://milvus:19530/db?token=" + secret + "#" + secret}, "http://milvus:19530/db"},
		{"unparseable", Config{DBEngine: "qdrant", QdrantURL: "user:" + secret + "@qdrant:6333"}, "redacted"},
		{"empty", Config{DBEngine: "qdrant"}, ""},
		{"pgsql never echoes DSN", Config{DBEngine: "pgsql", PgSQLDSN: "postgresql://recall:" + secret + "@pgsql:5432/recall"}, "pgsql"},
		{"unknown engine", Config{DBEngine: "other", QdrantURL: "https://u:" + secret + "@x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.BackendEndpoint()
			if got != tc.want {
				t.Fatalf("BackendEndpoint() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Fatalf("BackendEndpoint() leaked the credential: %q", got)
			}
		})
	}
}
