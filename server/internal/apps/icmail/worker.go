package icmail

import "context"

// runWorker hands control to the source runners. The loop itself lives in
// runner.go; this indirection keeps Start/Stop in app.go honest.
func (a *App) runWorker(ctx context.Context) {
	a.RunSources(ctx)
}