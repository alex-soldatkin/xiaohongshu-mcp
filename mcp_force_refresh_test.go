package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForceRefreshInToolSchemas pins the one intended difference WS2 makes to
// tools/list: the seven read tools gain a force_refresh property and nothing
// else does.
//
// The property comes from an embedded RefreshArgs, which jsonschema-go flattens
// into the parent object. Embedding is easy to lose in a refactor — an
// anonymous field turned into a named one still compiles and still marshals,
// it just stops being a tool argument.
func TestForceRefreshInToolSchemas(t *testing.T) {
	router := setupRoutes(NewAppServer(NewXiaohongshuService(), ""))
	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.NotEmpty(t, result.Result.Tools)

	readTools := map[string]bool{
		"list_feeds":         true,
		"search_feeds":       true,
		"get_feed_detail":    true,
		"user_profile":       true,
		"get_my_profile":     true,
		"get_unread_count":   true,
		"list_notifications": true,
	}

	seen := map[string]bool{}
	for _, tool := range result.Result.Tools {
		_, has := tool.InputSchema.Properties["force_refresh"]
		if readTools[tool.Name] {
			assert.True(t, has, "read tool %s must expose force_refresh", tool.Name)
			seen[tool.Name] = true
			continue
		}
		assert.False(t, has, "tool %s must not expose force_refresh", tool.Name)
	}
	assert.Len(t, seen, len(readTools), "every read tool must be registered")
}
