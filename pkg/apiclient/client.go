package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a running Fleetplane over HTTP.
type Client struct {
	base  string
	token string
	http  *http.Client
}

func New(base, token string) *Client {
	return &Client{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   ErrorDetail
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Body.Code, e.Body.Message)
}

func (c *Client) do(ctx context.Context, method, path string, in any, idemKey string, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var eb ErrorBody
		_ = json.Unmarshal(raw, &eb)
		return &APIError{Status: resp.StatusCode, Body: eb.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) CreateResource(ctx context.Context, req CreateResourceRequest, idemKey string) (*Resource, error) {
	var res Resource
	if err := c.do(ctx, http.MethodPost, "/v1/resources", req, idemKey, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) GetResource(ctx context.Context, id string) (*Resource, error) {
	var res Resource
	if err := c.do(ctx, http.MethodGet, "/v1/resources/"+id, nil, "", &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) ListResources(ctx context.Context) (*ResourceList, error) {
	var list ResourceList
	if err := c.do(ctx, http.MethodGet, "/v1/resources", nil, "", &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *Client) DeleteResource(ctx context.Context, id, idemKey string) error {
	return c.do(ctx, http.MethodDelete, "/v1/resources/"+id, nil, idemKey, nil)
}
