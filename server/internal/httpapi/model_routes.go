package httpapi

import (
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/models"
	"net/http"
	"strings"
)

func (s *WorkerRoutes) modelConfig() (map[string]any, error) {
	if s.options.ModelConfig != nil {
		return s.options.ModelConfig()
	}
	doc, a, e := s.fleet.Load()
	if e != nil {
		return nil, e
	}
	c := fleet.Object(fleet.Clone(doc.Defaults))
	for k, v := range doc.Hosts["nas"] {
		c[k] = fleet.Clone(v)
	}
	e = fleet.ApplyModels(c, a.Output["models"])
	return c, e
}
func (s *WorkerRoutes) modelRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true, Screen: true, CLI: true, RequireDelegation: true})
	if !ok {
		return
	}
	service := &models.Service{DB: s.options.DB, Now: s.options.Now}
	var value any
	var err error
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/model-overrides") {
		value, err = s.modelOverrideAction(r, service, path)
	} else {
		value, err = s.providerDisableAction(r, service, principal, path)
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

// modelOverrideAction は工程のモデル固定の一覧と、旧モデル切替の保存値の取消を扱う。
func (s *WorkerRoutes) modelOverrideAction(r *http.Request, service *models.Service, path string) (any, error) {
	var value any
	var err error
	var config map[string]any
	config, err = s.modelConfig()
	if err == nil {
		if path == "/api/model-overrides" {
			err = workerMethod(r, "GET")
			if err == nil {
				value, err = s.overridesView(r, config)
			}
		} else {
			parts := strings.Split(strings.TrimPrefix(path, "/api/model-overrides/"), "/")
			if len(parts) > 2 || (len(parts) == 2 && parts[1] != "cancel") {
				err = contract.NewProblem(404, "経路が見つかりません。", "")
			} else if parts[0] != "intake" && parts[0] != "review" && parts[0] != "priority" {
				err = contract.NewProblem(400, "工程は intake・review・priority のどれかです。", "")
			} else if err = workerMethod(r, "POST"); err == nil {
				_, err = workerBody(r)
				if err == nil {
					if len(parts) == 1 {
						err = contract.NewProblem(409, "工程の実行設定は固定規則で決まり、一時切り替えはできません。", "")
					} else {
						var cancelled bool
						cancelled, err = service.Cancel(r.Context(), "modelOverride."+parts[0])
						if err == nil {
							view, e := s.overridesView(r, config)
							err = e
							if err == nil {
								view["cancelled"] = cancelled
								value = view
							}
						}
					}
				}
			}
		}
	}
	return value, err
}

// providerDisableAction は実行先の一時的な無効の一覧・設定・取消を扱う。
func (s *WorkerRoutes) providerDisableAction(r *http.Request, service *models.Service, principal auth.Principal, path string) (any, error) {
	var value any
	var err error
	var ready []map[string]any
	if s.options.ModelProviders != nil {
		ready, err = s.options.ModelProviders(r.Context())
	}
	if err == nil {
		if path == "/api/provider-disables" {
			err = workerMethod(r, "GET")
			if err == nil {
				value, err = service.Disables(r.Context(), ready)
			}
		} else {
			parts := strings.Split(strings.TrimPrefix(path, "/api/provider-disables/"), "/")
			if len(parts) > 2 || (len(parts) == 2 && parts[1] != "cancel") {
				err = contract.NewProblem(404, "経路が見つかりません。", "")
			} else if parts[0] != "codex" && parts[0] != "claude" {
				err = contract.NewProblem(400, "無効にできる実行先は codex・claude です。", "")
			} else if err = workerMethod(r, "POST"); err == nil {
				var body map[string]any
				body, err = workerBody(r)
				if err == nil {
					var entry map[string]any
					var cancelled bool
					if len(parts) == 2 {
						cancelled, err = service.CancelDisable(r.Context(), parts[0])
					} else {
						entry, err = service.SetDisable(r.Context(), parts[0], body, principal.Name, ready)
					}
					if err == nil {
						var view map[string]any
						view, err = service.Disables(r.Context(), ready)
						if err != nil {
							return nil, err
						}
						if len(parts) == 2 {
							view["cancelled"] = cancelled
						} else {
							entry["provider"] = parts[0]
							view["set"] = entry
						}
						value = view
					}
				}
			}
		}
	}
	return value, err
}

func (s *WorkerRoutes) overridesView(r *http.Request, config map[string]any) (map[string]any, error) {
	view := models.OverridesView(config)
	priority, e := (&models.Service{DB: s.options.DB, Now: s.options.Now}).Resolve(r.Context(), config, map[string]any{}, "priority")
	if e != nil {
		return nil, e
	}
	fleet.Object(view["defaults"])["priority"] = priority
	return view, nil
}
