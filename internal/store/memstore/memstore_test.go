package memstore_test

import (
	"testing"

	"cloud-control/internal/store"
	"cloud-control/internal/store/memstore"
	"cloud-control/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return memstore.New() })
}
