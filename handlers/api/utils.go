package api

import "strings"

// Function to get domain from email
func GetDomainFromEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 {
		return parts[1]
	}
	return "localhost" // fallback
}

// Helper functions
func GetUsernameFromEmail(email string) string {
	parts := strings.Split(strings.TrimSpace(email), "@")
	if len(parts) == 2 && parts[0] != "" {
		return parts[0]
	}
	return ""
}
