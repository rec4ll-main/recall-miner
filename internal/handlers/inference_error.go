package handlers

import (
	"net/http"
	"recall-miner/internal/httpx"
)

// inferenceDependencyError applies only after dispatch to an inference backend.
// Provider status alone cannot attribute failure to our caller. Local validation
// errors bypass this mapping; generic RequestJSON users (including DBs) keep their
// contracts. Do not expose provider credentials, URLs, response bodies or inputs.
func inferenceDependencyError(err error) error {
	status := httpx.ErrorStatus(err, http.StatusBadGateway)
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return httpx.StatusError(http.StatusServiceUnavailable, "inference backend unavailable")
	}
	return httpx.StatusError(http.StatusBadGateway, "inference backend request failed")
}
