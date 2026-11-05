package runtime

import "fmt"

// BackendBindingProvider is the optional exact-runtime routing port. Binding
// selects an already-constructed backend; it grants no session or work authority.
type BackendBindingProvider interface {
	ValidateBackend(key string) error
	BoundBackend(name string) (string, error)
	Bind(name, key string) error
}

// ConditionalBackendBindingProvider lets transparent wrappers expose binding
// methods without claiming that an underlying legacy provider supports them.
type ConditionalBackendBindingProvider interface {
	HasBackendBindings() bool
}

// AsBackendBindingProvider returns the routing port only when it is supported.
func AsBackendBindingProvider(provider Provider) (BackendBindingProvider, bool) {
	if conditional, ok := provider.(ConditionalBackendBindingProvider); ok && !conditional.HasBackendBindings() {
		return nil, false
	}
	binder, ok := provider.(BackendBindingProvider)
	return binder, ok
}

// WithBackendCreation fences an exact router's creation against replacement.
// Legacy providers have no router publication lifetime and execute directly.
func WithBackendCreation(provider Provider, create func() error) error {
	if _, strict := AsBackendBindingProvider(provider); !strict {
		return create()
	}
	gate, ok := provider.(interface{ WithBackendCreation(func() error) error })
	if !ok {
		return fmt.Errorf("%w: runtime creation fence unavailable", ErrRuntimeUnavailable)
	}
	return gate.WithBackendCreation(create)
}
