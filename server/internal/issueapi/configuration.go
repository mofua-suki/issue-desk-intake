package issueapi

import "issue-desk-intake/server/internal/fleet"

func (s *Service) configuration() (Object, error) {
	if s.o.ConfigReader != nil {
		return s.o.ConfigReader()
	}
	return s.o.Config, nil
}

func (s *Service) detailAuthorChecked(issue Object) (bool, error) {
	config, e := s.configuration()
	if e != nil {
		return false, e
	}
	user := fleet.ObjectOrEmpty(issue["user"])
	dedicated := s.o.DedicatedAuthor
	if dedicated == nil {
		dedicated = s.o.Own
	}
	if dedicated != nil && dedicated(user) {
		return true, nil
	}
	return fleet.Text(user["login"]) != "" && user["login"] == config["gitUser"], nil
}
