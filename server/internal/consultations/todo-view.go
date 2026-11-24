package consultations

import "issue-desk-intake/server/internal/auth"

// TodoView applies the same image, URL and action projection used by the TODO API.
func (s *Service) TodoView(r Record, p auth.Principal, origin string) Record {
	return s.todoView(r, p, origin)
}
