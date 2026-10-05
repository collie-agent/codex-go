package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxResponseBytes = 1 << 20

func intDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func selectPage[T any](items []T, offset, limit int) ([]T, bool, *int) {
	if offset >= len(items) {
		return []T{}, false, nil
	}
	// Compare the remaining length first to avoid offset+limit overflow.
	if limit >= len(items)-offset {
		return items[offset:], false, nil
	}
	next := offset + limit
	return items[offset:next], true, &next
}

func toolResponse(result any, text string, isError bool) (*sdk.CallToolResult, error) {
	response := &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: text}},
		StructuredContent: result,
		IsError:           isError,
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxResponseBytes {
		return nil, fmt.Errorf("tool response exceeds 1 MiB limit")
	}
	return response, nil
}

// pageResponse fits both representations of a page into the full MCP response
// limit, retaining complete records and a continuation offset that advances.
func pageResponse[T, R any](items []T, offset int, truncated bool, nextOffset *int,
	makeResult func([]T, bool, *int) R, formatItem func(T) string,
) (*sdk.CallToolResult, R, error) {
	build := func(count int) (*sdk.CallToolResult, R, error) {
		cut, next := truncated, nextOffset
		if count < len(items) {
			cut = true
			value := offset + count
			next = &value
		}
		selected := items[:count]
		result := makeResult(selected, cut, next)
		var text strings.Builder
		for _, item := range selected {
			text.WriteString(formatItem(item))
			text.WriteByte('\n')
		}
		response, err := toolResponse(result, strings.TrimSuffix(text.String(), "\n"), false)
		return response, result, err
	}
	response, result, err := build(len(items))
	if err == nil {
		return response, result, nil
	}
	// Locate the largest fitting prefix without repeatedly removing one record.
	low, high := 1, len(items)-1
	var bestResponse *sdk.CallToolResult
	var bestResult R
	for low <= high {
		count := low + (high-low)/2
		candidate, value, err := build(count)
		if err != nil {
			high = count - 1
		} else {
			bestResponse, bestResult = candidate, value
			low = count + 1
		}
	}
	if bestResponse == nil {
		var zero R
		return nil, zero, fmt.Errorf("single record exceeds 1 MiB response limit: %w", err)
	}
	return bestResponse, bestResult, nil
}
