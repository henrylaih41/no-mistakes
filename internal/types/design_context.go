package types

import (
	"encoding/json"
	"fmt"
)

// DesignContext is the run-materialized design contract supplied by the user
// and/or repo config. Steps read this immutable copy rather than rereading
// mutable files during later fix rounds.
type DesignContext struct {
	Files []DesignContextFile `json:"files,omitempty"`
}

// DesignContextFile is one text file included in a run's design context.
type DesignContextFile struct {
	Source        string `json:"source"`
	Content       string `json:"content"`
	Truncated     bool   `json:"truncated,omitempty"`
	OriginalBytes int64  `json:"original_bytes,omitempty"`
}

// MarshalDesignContextJSON encodes a materialized design context for storage
// on a run. Empty contexts encode to an empty string.
func MarshalDesignContextJSON(ctx DesignContext) (string, error) {
	if len(ctx.Files) == 0 {
		return "", nil
	}
	data, err := json.Marshal(ctx)
	if err != nil {
		return "", fmt.Errorf("marshal design context: %w", err)
	}
	return string(data), nil
}

// ParseDesignContextJSON decodes a run's stored design-context JSON. Empty
// input returns a zero context.
func ParseDesignContextJSON(raw string) (DesignContext, error) {
	if raw == "" {
		return DesignContext{}, nil
	}
	var ctx DesignContext
	if err := json.Unmarshal([]byte(raw), &ctx); err != nil {
		return DesignContext{}, fmt.Errorf("parse design context: %w", err)
	}
	return ctx, nil
}
