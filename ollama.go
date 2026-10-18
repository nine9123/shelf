package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
		"model":   o.visM,
		"prompt":  prompt,
		"stream":  false,
		"images":  []string{base64.StdEncoding.EncodeToString(img)},
		"options": map[string]any{"num_predict": 120, "temperature": 0.2},
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

// HasModel reports whether a model (matched on the name before any ":tag") is
// pulled. Used to fail loudly when the vision model is missing instead of
// silently producing caption-less embeddings.
func (o *Ollama) HasModel(ctx context.Context, name string) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", o.url+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := o.hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()
	var r struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return false
	}
	want := baseModel(name)
	for _, m := range r.Models {
		if strings.EqualFold(baseModel(m.Name), want) {
			return true
		}
	}
	return false
}

func baseModel(n string) string {
	if i := strings.IndexByte(n, ':'); i >= 0 {
		return n[:i]
	}
	return n
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
		return &statusError{code: resp.StatusCode,
			msg: fmt.Sprintf("ollama %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusError is a non-200 answer from Ollama.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// unavailable reports whether err means Ollama couldn't be asked at all
// (connection refused, timeout, cancelled, gateway/starting-up status), as
// opposed to Ollama answering with an error about this particular input. Only
// the latter may be recorded as a result; the former must be retried later.
func unavailable(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return true // no HTTP answer: transport error, timeout or cancellation
	}
	switch se.code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}
