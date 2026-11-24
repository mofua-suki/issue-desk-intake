package issueapi

import "testing"

func checkedAuthor(t *testing.T, s *Service, issue Object) bool {
	t.Helper()
	allowed, err := s.requestAuthorChecked(issue)
	if err != nil {
		t.Fatal(err)
	}
	return allowed
}

func TestRequestAuthorUsesDedicatedIdentityAndExplicitAllowedUsers(t *testing.T) {
	s := &Service{o: Options{Config: Object{"gitUser": "old", "allowedUsers": []any{"allowed"}}, Own: func(u Object) bool { return u["id"] == float64(1) || u["id"] == float64(2) }, DedicatedAuthor: func(u Object) bool { return u["id"] == float64(1) }}}
	for _, entry := range []struct {
		user Object
		want bool
	}{{Object{"id": float64(1), "login": "renamed-intake"}, true}, {Object{"id": float64(99), "login": "intake"}, false}, {Object{"id": float64(2), "login": "old"}, false}, {Object{"id": float64(3), "login": "allowed"}, true}, {Object{"id": float64(4), "login": "Allowed"}, false}, {Object{}, false}} {
		if got := checkedAuthor(t, s, Object{"user": entry.user}); got != entry.want {
			t.Fatalf("author %v got=%v want=%v", entry.user, got, entry.want)
		}
	}
}

func TestRequestAuthorReadsUpdatedConfigAndFailsClosed(t *testing.T) {
	config := Object{"allowedUsers": []any{"first"}}
	s := &Service{o: Options{ConfigReader: func() (Object, error) { return config, nil }, DedicatedAuthor: func(Object) bool { return false }}}
	if !checkedAuthor(t, s, Object{"user": Object{"login": "first"}}) {
		t.Fatal("configured author rejected")
	}
	config = Object{"allowedUsers": []any{"second"}}
	if checkedAuthor(t, s, Object{"user": Object{"login": "first"}}) || !checkedAuthor(t, s, Object{"user": Object{"login": "second"}}) {
		t.Fatal("author configuration was stale")
	}
	s.o.ConfigReader = func() (Object, error) { return nil, fail(503, "担当表を読めません") }
	if _, e := s.requestAuthorChecked(Object{"user": Object{"login": "second"}}); e == nil {
		t.Fatal("unavailable source became a successful empty list")
	}
}
