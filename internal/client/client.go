// Package client is a small Go client for the burstyd API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/theognis1002/bursty/internal/api"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) *Client {
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{}}
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
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("burstyd unreachable at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiError turns an error response into an error, preferring the API's error message.
func apiError(resp *http.Response) error {
	var e api.Error
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
}

func (c *Client) Info(ctx context.Context) (api.Info, error) {
	var v api.Info
	return v, c.do(ctx, "GET", "/v1/info", nil, &v)
}

func (c *Client) CreateVM(ctx context.Context, req api.CreateVMRequest) (api.VM, error) {
	var v api.VM
	return v, c.do(ctx, "POST", "/v1/vms", req, &v)
}

func (c *Client) ListVMs(ctx context.Context) ([]api.VM, error) {
	var v []api.VM
	return v, c.do(ctx, "GET", "/v1/vms", nil, &v)
}

func (c *Client) GetVM(ctx context.Context, id string) (api.VM, error) {
	var v api.VM
	return v, c.do(ctx, "GET", "/v1/vms/"+url.PathEscape(id), nil, &v)
}

func (c *Client) DeleteVM(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/vms/"+url.PathEscape(id), nil, nil)
}

// VMAction runs stop, pause or resume.
func (c *Client) VMAction(ctx context.Context, id, action string) (api.VM, error) {
	var v api.VM
	return v, c.do(ctx, "POST", "/v1/vms/"+url.PathEscape(id)+"/"+action, nil, &v)
}

func (c *Client) Logs(ctx context.Context, id string, follow bool, w io.Writer) error {
	q := ""
	if follow {
		q = "?follow=1"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/v1/vms/"+url.PathEscape(id)+"/logs"+q, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func (c *Client) CreateSnapshot(ctx context.Context, vm string, req api.CreateSnapshotRequest) (api.Snapshot, error) {
	var v api.Snapshot
	return v, c.do(ctx, "POST", "/v1/vms/"+url.PathEscape(vm)+"/snapshots", req, &v)
}

func (c *Client) ListSnapshots(ctx context.Context) ([]api.Snapshot, error) {
	var v []api.Snapshot
	return v, c.do(ctx, "GET", "/v1/snapshots", nil, &v)
}

func (c *Client) DeleteSnapshot(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/snapshots/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Restore(ctx context.Context, snap string, req api.RestoreRequest) (api.VM, error) {
	var v api.VM
	return v, c.do(ctx, "POST", "/v1/snapshots/"+url.PathEscape(snap)+"/restore", req, &v)
}
