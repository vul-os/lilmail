package handlers

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func GetProjectRoot() string {
	_, b, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(b), "../../../")
	return projectRoot
}

func GetUsernameFromEmail(email string) (string, error) {
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" {
		return "", fmt.Errorf("invalid email format: %s", email)
	}
	return parts[0], nil
}
