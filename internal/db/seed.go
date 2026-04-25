package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

func (d *DB) SeedFromFile(ctx context.Context, filePath string) error {
	// Decode the seed file. Uses json.NewDecoder for streaming I/O reads,
	// but the full product array is materialized in memory.
	f, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return fmt.Errorf("failed to open seed file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var products []Product
	if err := json.NewDecoder(f).Decode(&products); err != nil {
		return fmt.Errorf("failed to parse seed file: %w", err)
	}

	slog.InfoContext(ctx, "seeding products", "count", len(products), "file", filePath)

	if err := d.UpsertProducts(ctx, products); err != nil {
		return fmt.Errorf("failed to seed products: %w", err)
	}

	slog.InfoContext(ctx, "seeding complete", "count", len(products))
	return nil
}
