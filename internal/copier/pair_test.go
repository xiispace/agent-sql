package copier

import (
	"context"
	"strings"
	"testing"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// markerServer is one fake server whose sessions share advisory markers.
type markerServer struct{ held map[int64]bool }

type fakeIntrospector struct {
	id     string
	server *markerServer
}

func (f *fakeIntrospector) Dialect() string                                { return "postgres" }
func (f *fakeIntrospector) ServerIdentity(context.Context) (string, error) { return f.id, nil }
func (f *fakeIntrospector) IntrospectSchema(context.Context, string, driver.IntrospectOpts) (*schemadiff.Model, error) {
	return nil, nil
}

func (f *fakeIntrospector) HoldMarker(_ context.Context, key int64) (func(), error) {
	f.server.held[key] = true
	return func() { delete(f.server.held, key) }, nil
}

func (f *fakeIntrospector) MarkerHeld(_ context.Context, key int64) (bool, error) {
	return f.server.held[key], nil
}

func TestSameTargetSeesThroughDifferentRoutes(t *testing.T) {
	ctx := context.Background()
	one := &markerServer{held: map[int64]bool{}}
	two := &markerServer{held: map[int64]bool{}}
	socket := &fakeIntrospector{id: "local:5432/app", server: one}
	tcp := &fakeIntrospector{id: "10.0.0.5:5432/app", server: one}
	elsewhere := &fakeIntrospector{id: "10.0.0.6:5432/app", server: two}

	err := SameTarget(ctx, socket, tcp, "public", "public")
	if err == nil || !strings.Contains(err.Error(), "same schema") {
		t.Errorf("socket vs TCP to one server: err = %v", err)
	}
	if len(one.held) != 0 {
		t.Errorf("marker not released: %v", one.held)
	}
	if err := SameTarget(ctx, socket, elsewhere, "public", "public"); err != nil {
		t.Errorf("different servers: %v", err)
	}
	if err := SameTarget(ctx, socket, tcp, "public", "other"); err != nil {
		t.Errorf("different schemas: %v", err)
	}
}
