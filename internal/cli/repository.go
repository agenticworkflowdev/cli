package cli

import (
	"context"
	"errors"
	"fmt"
)

func discoverRoot(ctx context.Context, services Services) (string, error) {
	if services.WorkingDirectory == nil || services.DiscoverRoot == nil {
		return "", errors.New("repository discovery is unavailable")
	}
	cwd, err := services.WorkingDirectory()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	root, err := services.DiscoverRoot(ctx, cwd)
	if err != nil {
		return "", fmt.Errorf("discover repository root: %w", err)
	}
	return root, nil
}
