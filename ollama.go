package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama is a thin client for a local Ollama server: text embeddings for search
// and image captioning for descriptions.
type Ollama struct {
	url    string
	embedM string
	visM   string
	hc     *http.Client
}

func NewOllama(url, embedModel, visionModel string) *Ollama {
	return &Ollama{
		url:    strings.TrimRight(url, "/"),
		embedM: embedModel,
		visM:   visionModel,
		hc:     &http.Client{Timeout: 5 * time.Minute}, // vision generation can be slow on a busy GPU
	}
}

// Embed returns the embedding vector for a piece of text.
func (o *Ollama) Embed(ctx context.Context, text string) ([]float32, error) {
	var r struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := o.post(ctx, "/api/embeddings", map[string]any{"model": o.embedM, "prompt": text}, &r); err != nil {
		return nil, err
	}
	if len(r.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding (is model %q pulled?)", o.embedM)
	}
	v := make([]float32, len(r.Embedding))
	for i, x := range r.Embedding {
		v[i] = float32(x)
	}
	return v, nil
}

// Caption asks the vision model to describe an image.
func (o *Ollama) Caption(ctx context.Context, img []byte, prompt string) (string, error) {
	var r struct {
		Response string `json:"response"`
	}
	body := map[string]any{
		"model":  o.visM,
		"prompt": prompt,
		"stream": false,
		"images": []string{base64.StdEncoding.EncodeToString(img)},
	}
	if err := o.post(ctx, "/api/generate", body, &r); err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Response), nil
}

// Reachable is a quick liveness probe.
func (o *Ollama) Reachable(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", o.url+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func (o *Ollama) post(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", o.url+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ollama %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
