package projects

import "context"

// Work survives the HTTP response while remaining cancellable by Service.Close.
// Existing gate values are retained when creation continues into initial filing.
func (s *Service) acquire(parent context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(s.ctx, cancel)
	clean := func() { stop(); cancel() }
	if s.o.Acquire == nil {
		return ctx, clean, nil
	}
	ctx, release, e := s.o.Acquire(ctx, "project")
	if e != nil {
		clean()
		return nil, nil, e
	}
	return ctx, func() { defer clean(); release() }, nil
}
