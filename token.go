package main

import (
	"errors"
	"os"
	"strings"
)

// GetDevToken reads the DEV_TOKEN file of an instance, when present.
func GetDevToken(instance *WrapperInstance) (string, error) {
	token, err := os.ReadFile(instanceDir(instance.Id) + "/DEV_TOKEN")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(token)), nil
}

// getAnyDevToken returns the first available instance DEV_TOKEN. wrapper-lite
// is the sole owner of token refresh; the manager only borrows a token for the
// one-time storefront-language lookup.
func getAnyDevToken() (string, error) {
	for _, inst := range SnapshotInstances() {
		if inst == nil {
			continue
		}
		token, err := GetDevToken(inst)
		if err == nil && token != "" {
			return token, nil
		}
	}
	return "", errors.New("no instance DEV_TOKEN available")
}
