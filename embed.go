package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"time"
)

// Ollama's embed endpoint. Local only — no network on the read path beyond this.
const ollamaURL = "http://localhost:11434/api/embed"

// embedBatchTimeout is generous because a 32-unit backfill batch can take a
// few seconds on the 0.6b model; the index path should never die mid-flight.
const embedBatchTimeout = 5 * time.Minute

// embedBatchSize keeps requests well under Ollama's payload limits while
// amortizing round-trips. Also the commit unit for `index`.
const embedBatchSize = 32

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

// embedBatch sends inputs in a single request. Ollama's API accepts an array
// and returns embeddings in matching order.
func embedBatch(inputs []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: embeddingModel, Input: inputs})
	if err != nil {
		return nil, err
	}
	cli := &http.Client{Timeout: embedBatchTimeout}
	resp, err := cli.Post(ollamaURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama unreachable at %s — %w\n"+
			"Start it with `ollama serve`, or pull the model with `ollama pull %s`",
			ollamaURL, err, embeddingModel)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned HTTP %d: %s\n"+
			"Confirm the model is pulled: `ollama list` (want %q)",
			resp.StatusCode, truncateForLog(raw), embeddingModel)
	}
	var r embedResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("cannot parse ollama response: %w", err)
	}
	if len(r.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama returned %d embeddings for %d inputs",
			len(r.Embeddings), len(inputs))
	}
	for _, e := range r.Embeddings {
		if len(e) != embeddingDim {
			return nil, fmt.Errorf("ollama returned %d-dim vector, expected %d (model mismatch?)",
				len(e), embeddingDim)
		}
	}
	return r.Embeddings, nil
}

// embedOne embeds a single string (query path, add path).
func embedOne(text string) ([]float32, error) {
	out, err := embedBatch([]string{text})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// queryPrefix is qwen3-embedding's retrieval instruction. It goes on queries
// only — stored units are the documents, and the model is trained with the
// instruction on one side of the pair, not both. Measured in Phase 2 on
// golden.txt: 12/14 without it, 13/14 with. Change it and re-run `memdb eval`.
const queryPrefix = "Instruct: Given a question about a past software decision, retrieve the conversation that decided it\nQuery: "

// embedQuery embeds a search query with the retrieval instruction attached.
func embedQuery(q string) ([]float32, error) {
	return embedOne(queryPrefix + q)
}

// embedMany batches a backfill through the API. Batch size is small enough to
// keep requests well under Ollama's payload limits, large enough to amortize
// round-trips. ~460 units → ~15 requests.
func embedMany(texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += embedBatchSize {
		j := i + embedBatchSize
		if j > len(texts) {
			j = len(texts)
		}
		emb, err := embedBatch(texts[i:j])
		if err != nil {
			return nil, fmt.Errorf("batch at offset %d: %w", i, err)
		}
		out = append(out, emb...)
	}
	return out, nil
}

// cosine is standard cosine similarity. Vectors are L2-normalish from Ollama
// but we don't trust that — full norm divide. ~460 rows × 1024 dims is
// sub-millisecond on any modern CPU, so no precompute.
func cosine(a, b []float32) float32 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var dot, na, nb float32
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(na))*math.Sqrt(float64(nb)))
}

// hit is one search result. Raw is pre-boost cosine (the Phase 5 threshold
// signal); Score is post-boost and drives ranking.
type hit struct {
	Mem   memory
	Raw   float32
	Score float32
}

// searchRows embeds nothing — caller passes the query vector. Brute force over
// the in-memory set, optional same-repo boost (pass 1.0 to disable), return
// top-K sorted desc by Score. cwdRepo empty disables the boost regardless.
func searchRows(rows []memory, qvec []float32, cwdRepo string, topK int, boost float32) []hit {
	hits := make([]hit, 0, len(rows))
	for _, m := range rows {
		raw := cosine(qvec, m.Vector)
		s := raw
		if cwdRepo != "" && m.Repo == cwdRepo && boost != 0 {
			s *= boost
		}
		hits = append(hits, hit{Mem: m, Raw: raw, Score: s})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Mem.ID < hits[j].Mem.ID // stable tiebreak
	})
	if topK >= 0 && topK < len(hits) {
		hits = hits[:topK]
	}
	return hits
}

func truncateForLog(b []byte) string {
	const max = 300
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
