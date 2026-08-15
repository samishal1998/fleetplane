package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

func (c *Client) Acquire(ctx context.Context, req AcquireRequest, idemKey string) (*Acquisition, error) {
	var acq Acquisition
	if err := c.do(ctx, http.MethodPost, "/v1/acquisitions", req, idemKey, &acq); err != nil {
		return nil, err
	}
	return &acq, nil
}

func (c *Client) GetAcquisition(ctx context.Context, id string) (*Acquisition, error) {
	var acq Acquisition
	if err := c.do(ctx, http.MethodGet, "/v1/acquisitions/"+id, nil, "", &acq); err != nil {
		return nil, err
	}
	return &acq, nil
}

func (c *Client) Release(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/acquisitions/"+id, nil, "", nil)
}

func (c *Client) DrainResource(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/resources/"+id+":drain", nil, "", nil)
}

func (c *Client) ApplyPool(ctx context.Context, m PoolManifest) (*Pool, error) {
	var pool Pool
	if err := c.do(ctx, http.MethodPost, "/v1/pools", m, "", &pool); err != nil {
		return nil, err
	}
	return &pool, nil
}

func (c *Client) GetPool(ctx context.Context, id string) (*Pool, error) {
	var pool Pool
	if err := c.do(ctx, http.MethodGet, "/v1/pools/"+id, nil, "", &pool); err != nil {
		return nil, err
	}
	return &pool, nil
}

func (c *Client) ListPools(ctx context.Context) (*PoolList, error) {
	var list PoolList
	if err := c.do(ctx, http.MethodGet, "/v1/pools", nil, "", &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *Client) ReconcilePool(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/pools/"+id+":reconcile", nil, "", nil)
}

func (c *Client) GetOperation(ctx context.Context, id string) (*Operation, error) {
	var op Operation
	if err := c.do(ctx, http.MethodGet, "/v1/operations/"+id, nil, "", &op); err != nil {
		return nil, err
	}
	return &op, nil
}

func (c *Client) ListOperations(ctx context.Context) ([]Operation, error) {
	var out struct{ Items []Operation }
	if err := c.do(ctx, http.MethodGet, "/v1/operations", nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) ListEvents(ctx context.Context, after, since string, limit int) ([]Event, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if since != "" {
		q.Set("since", since)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/events"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out struct{ Items []Event }
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ProviderHealth mirrors the /v1/providers item shape.
type ProviderHealth struct {
	Instance            string `json:"instance"`
	Driver              string `json:"driver"`
	State               string `json:"state"`
	LastError           string `json:"lastError,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
}

func (c *Client) ListProviders(ctx context.Context) ([]ProviderHealth, error) {
	var out struct{ Items []ProviderHealth }
	if err := c.do(ctx, http.MethodGet, "/v1/providers", nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// --- classes (dynamic classes) ---

func (c *Client) CreateClass(ctx context.Context, m ClassManifest) (*Class, error) {
	var out Class
	if err := c.do(ctx, http.MethodPost, "/v1/classes", m, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpsertClass(ctx context.Context, name string, m ClassManifest) (*Class, error) {
	var out Class
	if err := c.do(ctx, http.MethodPut, "/v1/classes/"+url.PathEscape(name), m, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetClass(ctx context.Context, name string) (*Class, error) {
	var out Class
	if err := c.do(ctx, http.MethodGet, "/v1/classes/"+url.PathEscape(name), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListClasses(ctx context.Context) ([]Class, error) {
	var out ClassList
	if err := c.do(ctx, http.MethodGet, "/v1/classes", nil, "", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) DeleteClass(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/classes/"+url.PathEscape(name), nil, "", nil)
}

// --- parked machines (docs/12) ---

func (c *Client) ParkResource(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/resources/"+url.PathEscape(id)+":park", nil, "", nil)
}

func (c *Client) StartResource(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/resources/"+url.PathEscape(id)+":start", nil, "", nil)
}
