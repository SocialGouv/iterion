package delegate

import "strings"

// ExplicitCredentialFailure separates a rejected credential from a model
// restriction on an otherwise valid credential. HTTP status alone is ambiguous.
func ExplicitCredentialFailure(message string) bool {
	msg := strings.ToLower(message)
	for _, needle := range []string{
		"authentication token is expired", "please try signing in", "invalid api key",
		"incorrect api key", "invalid_api_key", "invalid x-api-key", "authentication_error",
		"oauth token has expired", "invalid bearer token", "failed to authenticate",
		"api key disabled", "api key revoked", "invalid proxy server token",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// ModelAccessDenied reads the same access refusal for gateway and CLI routes.
// Legacy gateways use 401 for model restrictions; both statuses need explicit
// model evidence and must preserve a genuine credential failure.
func ModelAccessDenied(status int, message string) bool {
	if (status != 401 && status != 403) || ExplicitCredentialFailure(message) {
		return false
	}
	msg := strings.ToLower(message)
	for _, needle := range []string{
		"model_access_denied", "not allowed to access model", "is not available for this api key",
		"does not have access to model", "model_not_found",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return status == 403 && strings.Contains(msg, "model not available")
}
