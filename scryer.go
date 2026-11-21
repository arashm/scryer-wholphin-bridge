package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

type gqlErr struct {
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"extensions"`
}

// scryerError is a GraphQL-level error. The data that did resolve is still decoded,
// so callers can tolerate failures on aliases they have a fallback for.
type scryerError struct{ errs []gqlErr }

func (e *scryerError) Error() string {
	msgs := make([]string, len(e.errs))
	for i, x := range e.errs {
		msgs[i] = x.Message
	}
	return strings.Join(msgs, "; ")
}

func (e *scryerError) status() int {
	switch e.errs[0].Extensions.Code {
	case "AUTHENTICATION_REQUIRED":
		return http.StatusUnauthorized
	case "UNAUTHORIZED":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// onlyOn reports whether every error is on one of the given top-level aliases.
func (e *scryerError) onlyOn(aliases ...string) bool {
	for _, x := range e.errs {
		if len(x.Path) == 0 || !contains(aliases, fmt.Sprint(x.Path[0])) {
			return false
		}
	}
	return true
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// gql runs one GraphQL document against Scryer with the caller's API key.
func gql(ctx context.Context, token, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scryerURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlErr        `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("scryer: HTTP %d: %w", resp.StatusCode, err)
	}
	if len(r.Data) > 0 && string(r.Data) != "null" && out != nil {
		if err := json.Unmarshal(r.Data, out); err != nil {
			return err
		}
	}
	if len(r.Errors) > 0 {
		return &scryerError{r.Errors}
	}
	return nil
}
