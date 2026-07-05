// Package embed defines the Embedder interface (spec §14.2) and adapters:
// a deterministic local feature-hashing embedder (dev/CI default — no
// external service needed for a fully working pipeline) and a Voyage AI
// HTTP adapter (production default per spec §4.4).
package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/internal/config"
)

// Embedder embeds texts into fixed-dimension vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	ModelID() string
	Dim() int
}

// New builds the configured embedder.
func New(m config.ModelRef) (Embedder, error) {
	dim := m.Dim
	if dim == 0 {
		dim = 1024
	}
	switch m.Provider {
	case "", "local":
		return NewHashEmbedder(dim), nil
	case "voyage":
		key := os.Getenv("VOYAGE_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("embedder provider voyage: VOYAGE_API_KEY not set")
		}
		return &voyageEmbedder{model: m.Model, dim: dim, key: key, hc: &http.Client{Timeout: 60 * time.Second}}, nil
	default:
		return nil, fmt.Errorf("unknown embedder provider %q", m.Provider)
	}
}

// ---------------------------------------------------------------------------
// Local hash embedder
// ---------------------------------------------------------------------------

// HashEmbedder is a deterministic bag-of-tokens feature-hashing embedder.
// It has no semantic knowledge but provides real, stable cosine geometry:
// texts sharing vocabulary land near each other. Good enough for dev, CI,
// and dedup-at-propose; swap for voyage-code-3 in production.
type HashEmbedder struct{ dim int }

func NewHashEmbedder(dim int) *HashEmbedder { return &HashEmbedder{dim: dim} }

func (h *HashEmbedder) ModelID() string { return fmt.Sprintf("local/hash-embedder-v1@%d", h.dim) }
func (h *HashEmbedder) Dim() int        { return h.dim }

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_]+`)

func (h *HashEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, h.dim)
		toks := tokenRe.FindAllString(strings.ToLower(t), -1)
		for j, tok := range toks {
			addFeature(v, tok, 1.0)
			if j > 0 { // bigrams give a little phrase sensitivity
				addFeature(v, toks[j-1]+"_"+tok, 0.5)
			}
		}
		normalize(v)
		out[i] = v
	}
	return out, nil
}

func addFeature(v []float32, feature string, weight float32) {
	sum := sha256.Sum256([]byte(feature))
	idx := binary.BigEndian.Uint32(sum[0:4]) % uint32(len(v))
	sign := float32(1)
	if sum[4]&1 == 1 {
		sign = -1
	}
	v[idx] += sign * weight
}

func normalize(v []float32) {
	var ss float64
	for _, x := range v {
		ss += float64(x) * float64(x)
	}
	if ss == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(ss))
	for i := range v {
		v[i] *= inv
	}
}

// Cosine returns the cosine similarity of two vectors.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// VectorLiteral serializes a vector as pgvector text input: "[x1,x2,...]".
func VectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', 6, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// ---------------------------------------------------------------------------
// Voyage adapter
// ---------------------------------------------------------------------------

type voyageEmbedder struct {
	model string
	dim   int
	key   string
	hc    *http.Client
}

func (v *voyageEmbedder) ModelID() string { return "voyage/" + v.model }
func (v *voyageEmbedder) Dim() int        { return v.dim }

func (v *voyageEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model": v.model, "input": texts, "output_dimension": v.dim,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.voyageai.com/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voyage embeddings: status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	return out, nil
}
