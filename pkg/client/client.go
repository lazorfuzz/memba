// Package client is the Go client for the memba /v1 API, used by
// connectors, benchmarks, and agent runtimes (spec §14.1).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lazorfuzz/memba/pkg/memory"
)

type Client struct {
	BaseURL string
	Token   string
	HC      *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token, HC: &http.Client{Timeout: 120 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var p memory.Problem
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(data, &p) == nil && p.Title != "" {
			return fmt.Errorf("%s %s: %d %s: %s", method, path, resp.StatusCode, p.Title, p.Detail)
		}
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) InsertEvidence(ctx context.Context, req memory.InsertRequest) (memory.InsertResponse, error) {
	var out memory.InsertResponse
	err := c.do(ctx, http.MethodPost, "/v1/evidence", req, &out)
	return out, err
}

func (c *Client) Query(ctx context.Context, req memory.QueryRequest) (memory.EvidencePack, error) {
	var out memory.EvidencePack
	err := c.do(ctx, http.MethodPost, "/v1/query", req, &out)
	return out, err
}

func (c *Client) Verify(ctx context.Context, req memory.VerifyRequest) (memory.VerificationResult, error) {
	var out memory.VerificationResult
	err := c.do(ctx, http.MethodPost, "/v1/verify", req, &out)
	return out, err
}

func (c *Client) Propose(ctx context.Context, req memory.ProposalRequest) (memory.ProposalResponse, error) {
	var out memory.ProposalResponse
	err := c.do(ctx, http.MethodPost, "/v1/cards", req, &out)
	return out, err
}

func (c *Client) GetCard(ctx context.Context, id string) (memory.Card, []memory.VerificationResult, error) {
	var out struct {
		Card          memory.Card                 `json:"card"`
		Verifications []memory.VerificationResult `json:"verifications"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/cards/"+id, nil, &out)
	return out.Card, out.Verifications, err
}

func (c *Client) ListCards(ctx context.Context, namespace, status string, limit int) ([]memory.Card, error) {
	q := url.Values{}
	if namespace != "" {
		q.Set("namespace", namespace)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Cards []memory.Card `json:"cards"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/cards?"+q.Encode(), nil, &out)
	return out.Cards, err
}

func (c *Client) Review(ctx context.Context, cardID string, req memory.ReviewRequest) (memory.ReviewResponse, error) {
	var out memory.ReviewResponse
	err := c.do(ctx, http.MethodPost, "/v1/cards/"+cardID+"/review", req, &out)
	return out, err
}

func (c *Client) Invalidate(ctx context.Context, cardID string, req memory.InvalidateRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/cards/"+cardID+"/invalidate", req, nil)
}

func (c *Client) InsertFact(ctx context.Context, f memory.Fact) (string, error) {
	var out struct {
		FactID string `json:"fact_id"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/facts", f, &out)
	return out.FactID, err
}

func (c *Client) RecordAction(ctx context.Context, a memory.MemoryAction) error {
	return c.do(ctx, http.MethodPost, "/v1/actions", a, nil)
}

func (c *Client) UpsertNamespace(ctx context.Context, ns memory.Namespace) error {
	return c.do(ctx, http.MethodPost, "/v1/admin/namespaces", ns, nil)
}

func (c *Client) Profile(ctx context.Context, namespace, repo string, level int) (string, error) {
	q := url.Values{"namespace": {namespace}, "repo": {repo}, "level": {strconv.Itoa(level)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/profile?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (c *Client) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %d", resp.StatusCode)
	}
	return nil
}
