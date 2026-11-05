package main

import (
	"context"
	"fmt"
	"io"
	"reflect"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// standaloneRuntimeCreationHandle fences independent CLI creators against the
// accepted controller registry. Existing-owner commands retain their provider.
// A live controller owns managed allocation; a standalone creator must hold its
// exclusive city lock and revalidate its captured configuration through commit.
type standaloneRuntimeCreationHandle struct {
	worker.Handle
	cityPath string
	cfg      *config.City
}

func (h *standaloneRuntimeCreationHandle) Create(ctx context.Context, mode worker.CreateMode) (session.Info, error) {
	var info session.Info
	err := withStandaloneRuntimeCreation(h.cityPath, h.cfg, func() error {
		var err error
		info, err = h.Handle.Create(ctx, mode)
		return err
	})
	return info, err
}

func withStandaloneRuntimeCreation(cityPath string, cfg *config.City, create func() error) error {
	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		return fmt.Errorf("%w: standalone runtime creation requires exclusive city ownership; use the controller API for managed allocation: %w", runtime.ErrRuntimeUnavailable, err)
	}
	defer lock.Close() //nolint:errcheck // release the exact exclusive lock
	current, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("%w: reloading standalone creation config: %w", runtime.ErrRuntimeUnavailable, err)
	}
	if !reflect.DeepEqual(current, cfg) {
		return fmt.Errorf("%w: standalone creation configuration changed; retry", runtime.ErrRuntimeUnavailable)
	}
	return create()
}
