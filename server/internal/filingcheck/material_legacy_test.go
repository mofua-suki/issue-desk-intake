package filingcheck

import "context"

// legacyMaterial uses the same material construction with the former full read.
func (s *Service) legacyMaterial(ctx context.Context, d Record) (Record, error) {
	drafts, err := s.options.Records.List(ctx, "drafts")
	if err != nil {
		return nil, err
	}
	return s.materialWithDrafts(ctx, d, drafts)
}
