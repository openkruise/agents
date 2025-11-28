// Package models provides data models for the E2B sandbox API.
package models

// Error represents an error response
type Error struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}
