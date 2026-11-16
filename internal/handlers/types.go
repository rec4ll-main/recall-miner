package handlers

import "encoding/json"

type dbCreateRequest struct {
	Name      string `json:"name"`
	Dimension int    `json:"dimension"`
	Backend   string `json:"backend,omitempty"`
}

type dbRestoreRequest struct {
	Name      string            `json:"name"`
	Dimension int               `json:"dimension,omitempty"`
	Reset     bool              `json:"reset,omitempty"`
	Vectors   [][]float64       `json:"vectors,omitempty"`
	Records   []dbRestoreRecord `json:"records,omitempty"`
}

type dbRestoreRecord struct {
	EmbedID  string         `json:"embed_id"`
	Vector   []float64      `json:"vector"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type dbQueryRequest struct {
	Text   string    `json:"text"`
	Vector []float64 `json:"vector,omitempty"`
	TopK   int       `json:"top_k"`
	Model  string    `json:"model,omitempty"`
	// Filter and Namespace are not supported. They are decoded only so a
	// request carrying them is rejected instead of silently widening scope.
	Filter    json.RawMessage `json:"filter,omitempty"`
	Namespace json.RawMessage `json:"namespace,omitempty"`
}

type embedRequest struct {
	Text  string `json:"text"`
	Model string `json:"model,omitempty"`
}

type embedBatchRequest struct {
	Texts []string `json:"texts"`
	Model string   `json:"model,omitempty"`
}

type storeEmbedRequest struct {
	Vector   []float64      `json:"vector"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type rerankRequest struct {
	Query        string      `json:"query"`
	Texts        []string    `json:"texts,omitempty"`
	Candidates   [][]float64 `json:"candidates,omitempty"`
	CandidateIDs []string    `json:"candidate_ids,omitempty"`
}

type setModelRequest struct {
	Model          string `json:"model"`
	DownloadURL    string `json:"download_url,omitempty"`
	DownloadPath   string `json:"download_path,omitempty"`
	DownloadSource string `json:"download_source,omitempty"`
}

type embedResult struct {
	Vector    []float64 `json:"vector"`
	Model     string    `json:"model"`
	Dimension int       `json:"dimension"`
}

type generateRequest struct {
	RequestKey       string `json:"request_key,omitempty"`
	scopedRequestKey string
	ProbeContract    string            `json:"probe_contract,omitempty"`
	Query            string            `json:"query"`
	Contexts         []generateContext `json:"contexts"`
	Model            string            `json:"model"`
	MaxTokens        int               `json:"max_tokens"`
	Temperature      *float64          `json:"temperature"`
}

type generateContext struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}
