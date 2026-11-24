package filing

import "sync"

func (s *Service) Busy(id string) bool {
	v, ok := s.drafts.Load(id)
	if !ok {
		return false
	}
	m := v.(*sync.Mutex)
	if !m.TryLock() {
		return true
	}
	m.Unlock()
	return false
}
