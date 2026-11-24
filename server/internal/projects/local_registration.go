package projects

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func (r *Registration) local(entry Object) (Object, []byte, bool, error) {
	raw, e := os.ReadFile(r.o.ConfigFile)
	if e != nil {
		return nil, nil, false, e
	}
	config, e := parseObject(string(raw))
	if e != nil {
		return nil, nil, false, e
	}
	if _, ok := config["repositories"].([]any); !ok {
		return nil, nil, false, fmt.Errorf("registration repository list invalid")
	}
	for _, v := range fleet.List(config["repositories"]) {
		if strings.EqualFold(fleet.Text(fleet.ObjectOrEmpty(v)["name"]), fleet.Text(entry["name"])) {
			return config, raw, false, nil
		}
	}
	config["repositories"] = append(fleet.List(config["repositories"]), clone(entry))
	if e = r.o.ValidateConfig(config); e != nil {
		return nil, nil, false, e
	}
	return config, raw, true, nil
}
func (r *Registration) checkLocal(entry Object) error { _, _, _, e := r.local(entry); return e }
func (r *Registration) Registered(_ context.Context, entry Object) error {
	if r.o.ConfigFile == "" || r.o.RuntimeDir == "" || r.o.ValidateConfig == nil {
		return fmt.Errorf("local registration configuration incomplete")
	}
	control := filepath.Join(r.o.RuntimeDir, "control")
	if e := os.MkdirAll(control, 0700); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(control, "project-registration.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	config, before, changed, e := r.local(entry)
	if e != nil {
		return e
	}
	if changed {
		if e = r.replaceConfig(control, before, config); e != nil {
			return e
		}
	}
	if r.o.Reload != nil {
		return r.o.Reload(config)
	}
	return nil
}

// replaceConfig は登録前の設定を控えてから、読んだ時から変わっていないことを確かめて設定ファイルを入れ替え、ディレクトリまで同期する。
func (r *Registration) replaceConfig(control string, before []byte, config Object) error {
	backup := filepath.Join(control, "config-before-register-"+uuid()+".json")
	if e := os.WriteFile(backup, before, 0600); e != nil {
		return e
	}
	tmp, e := os.CreateTemp(filepath.Dir(r.o.ConfigFile), ".project-register-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(append(jsonBytes(config), '\n'))
	}
	if e == nil {
		e = tmp.Sync()
	}
	closeErr := tmp.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	current, e := os.ReadFile(r.o.ConfigFile)
	if e != nil {
		return e
	}
	if string(current) != string(before) {
		return fmt.Errorf("configuration changed during registration")
	}
	if e = os.Rename(name, r.o.ConfigFile); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(r.o.ConfigFile))
	if e != nil {
		return e
	}
	e = dir.Sync()
	_ = dir.Close()
	if e != nil {
		return e
	}
	return nil
}
