package release

import "context"

// Registrar is the callback into library registration used by release
// monitoring. It decouples the Scheduler from the host runtime client:
// the host wires its own registrar implementation rather than the
// release package importing a runtimehost or plugin SDK type.
type Registrar interface {
	Register(ctx context.Context, imdbID string) error
	Update(ctx context.Context, imdbID string) error
}
