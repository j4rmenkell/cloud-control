package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"cloud-control/internal/store"
	"cloud-control/internal/store/storetest"
)

func TestSQLiteContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
