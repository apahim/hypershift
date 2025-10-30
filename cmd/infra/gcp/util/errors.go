package util

import (
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/googleapi"
)

// GCP-specific error handling
func IsGCPError(err error, code int) bool {
	if gerr, ok := err.(*googleapi.Error); ok {
		return gerr.Code == code
	}
	return false
}

func IsGCPNotFoundError(err error) bool {
	return IsGCPError(err, http.StatusNotFound)
}

func IsGCPConflictError(err error) bool {
	return IsGCPError(err, http.StatusConflict)
}

func IsGCPQuotaError(err error) bool {
	if gerr, ok := err.(*googleapi.Error); ok {
		return gerr.Code == http.StatusTooManyRequests ||
			strings.Contains(gerr.Message, "quota") ||
			strings.Contains(gerr.Message, "Quota")
	}
	return false
}

func IsGCPPermissionError(err error) bool {
	if gerr, ok := err.(*googleapi.Error); ok {
		return gerr.Code == http.StatusForbidden ||
			gerr.Code == http.StatusUnauthorized
	}
	return false
}

func FormatGCPError(err error, operation string) error {
	if gerr, ok := err.(*googleapi.Error); ok {
		switch gerr.Code {
		case http.StatusNotFound:
			return fmt.Errorf("%s: resource not found", operation)
		case http.StatusConflict:
			return fmt.Errorf("%s: resource already exists or conflict detected", operation)
		case http.StatusForbidden:
			return fmt.Errorf("%s: insufficient permissions", operation)
		case http.StatusUnauthorized:
			return fmt.Errorf("%s: authentication failed", operation)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%s: rate limit exceeded", operation)
		default:
			return fmt.Errorf("%s: %s (code: %d)", operation, gerr.Message, gerr.Code)
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// Resource-specific error handling
func HandleResourceCreationError(err error, resourceType, resourceName string) error {
	if IsGCPConflictError(err) {
		return fmt.Errorf("%s %s already exists", resourceType, resourceName)
	}
	if IsGCPPermissionError(err) {
		return fmt.Errorf("insufficient permissions to create %s %s", resourceType, resourceName)
	}
	if IsGCPQuotaError(err) {
		return fmt.Errorf("quota exceeded while creating %s %s", resourceType, resourceName)
	}
	return FormatGCPError(err, fmt.Sprintf("creating %s %s", resourceType, resourceName))
}

func HandleResourceDeletionError(err error, resourceType, resourceName string) error {
	if IsGCPNotFoundError(err) {
		// Resource doesn't exist, consider it successfully deleted
		return nil
	}
	if IsGCPPermissionError(err) {
		return fmt.Errorf("insufficient permissions to delete %s %s", resourceType, resourceName)
	}
	return FormatGCPError(err, fmt.Sprintf("deleting %s %s", resourceType, resourceName))
}
