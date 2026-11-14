// Package relay forwards framed USB requests without keeping attendance or Discord state.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"office/internal/service"
	"time"
)

type Client struct {
	URL    string
	APIKey string
	HTTP   *http.Client
}

func (c *Client) Handle(ctx context.Context, req service.ReaderRequest) service.ReaderResponse {
	fail := service.ReaderResponse{Version: 1, ID: req.ID, Status: 503, Error: "backend unavailable"}
	body, err := json.Marshal(req)
	if err != nil {
		return fail
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return fail
	}
	r.Header.Set("X-API-Key", c.APIKey)
	r.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(r)
	if err != nil {
		return fail
	}
	defer response.Body.Close()
	var reply service.ReaderResponse
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&reply) != nil || reply.Version != 1 || reply.ID != req.ID || reply.Status != response.StatusCode || reply.Status < 200 || reply.Status > 599 {
		return fail
	}
	return reply
}
