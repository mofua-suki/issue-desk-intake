package adminapi

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ホスト作業用SSHの方針。他マシンへのSSHは初期状態で全部拒否し、利用者の明示の許可（受付画面の承認、
// または利用者の発言を引用した host-ssh の許可付与）があるときだけ発券する。
// このファイルは方針の読取と検査を担当し、申請の承認・発券は別の処理で判断する。
// Principal は署名役（NASの /usr/local/libexec/issue-desk-ssh-sign、root所有）と各PCのsshdの設定が決めた名前で、
// issue-desk-repair-<PC名> の形に固定されている。名前を変えるときは、署名役と各PCの設定も同時に直す。
type hostSSHHost struct {
	Principal string `json:"principal"`
	User      string `json:"user"`
	Machine   string `json:"machine"`
}
type directPRVerification struct {
	Requester string `json:"requester"`
	Host      string `json:"host"`
}

type hostSSHPolicy struct {
	TTL    int `json:"ttlMinutes"`
	Signer struct {
		Token string `json:"token"`
	} `json:"signer"`
	Hosts                map[string]hostSSHHost          `json:"hosts"`
	Operations           map[string]string               `json:"operations"`
	ReadOnlyOperations   []string                        `json:"readOnlyOperations"`
	OperationHosts       map[string][]string             `json:"operationHosts"`
	DirectPRVerification map[string]directPRVerification `json:"directPRVerification"`
}

// UnmarshalJSON distinguishes an omitted restriction from an explicit null.
func (p *hostSSHPolicy) UnmarshalJSON(data []byte) error {
	type policy hostSSHPolicy
	var decoded policy
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["operationHosts"]; ok && (string(raw) == "null" || len(decoded.OperationHosts) == 0) {
		return bad(503, "ホスト作業用SSHの設定が不正です。")
	}
	*p = hostSSHPolicy(decoded)
	return nil
}

func (p *hostSSHPolicy) allows(host, operation string) bool {
	if _, ok := p.Hosts[host]; !ok || p.Operations[operation] == "" {
		return false
	}
	hosts, restricted := p.OperationHosts[operation]
	return !restricted || contains(hosts, host)
}

func (p *hostSSHPolicy) allowsDirectPRVerify(repo, requester, host, operation string) bool {
	entry, ok := p.DirectPRVerification[repo]
	return ok && operation == "verify" && entry.Requester == requester && entry.Host == host && p.allows(host, operation)
}

var pcName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// machine は ssh が名前解決できる名前。NASは個人のSSH設定（-F /dev/null）を使わないので、別名ではなく
// Tailscaleの完全な名前（<PC名>.example.ts.net）を書く。
var machineName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

func singleText(v string) bool {
	return strings.TrimSpace(v) != "" && !strings.ContainsAny(v, "\r\n\x00")
}

// hostSSHPolicyFile は方針ファイルの場所を返す。旧キー sshRepairPolicyFile と旧ファイル名 ssh-repair.json は、
// 配備済みの設定を壊さないための読み替え（設定を hostSshPolicyFile へ直したら外してよい）。
func (s *service) hostSSHPolicyFile() string {
	file := str(s.Config["hostSshPolicyFile"])
	if file == "" {
		file = str(s.Config["sshRepairPolicyFile"])
	}
	if filepath.Base(file) == "ssh-repair.json" {
		if _, e := os.Stat(file); e != nil {
			return filepath.Join(filepath.Dir(file), "host-ssh.json")
		}
	}
	return file
}
func (s *service) repairPolicy() (*hostSSHPolicy, error) {
	file := s.hostSSHPolicyFile()
	if file == "" {
		return nil, bad(503, "ホスト作業用SSHの設定がありません。")
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		return nil, bad(503, "ホスト作業用SSHの設定を読めません。")
	}
	var p hostSSHPolicy
	if json.Unmarshal(raw, &p) != nil || !p.valid() {
		return nil, bad(503, "ホスト作業用SSHの設定が不正です。")
	}
	return &p, nil
}

// valid checks the decoded policy: the TTL and signer, each host's fixed
// principal and machine name, each operation's command, and that read-only
// operations are distinct registered operations.
func (p *hostSSHPolicy) valid() bool {
	if p.TTL < 1 || p.TTL > 60 || !pcName.MatchString(p.Signer.Token) || len(p.Hosts) == 0 || len(p.Operations) == 0 {
		return false
	}
	for host, v := range p.Hosts {
		if !pcName.MatchString(host) || v.Principal != "issue-desk-repair-"+host || !singleText(v.User) || !machineName.MatchString(v.Machine) {
			return false
		}
	}
	for operation, command := range p.Operations {
		if !pcName.MatchString(operation) || !singleText(command) || len(command) > 4000 {
			return false
		}
	}
	for operation, hosts := range p.OperationHosts {
		if p.Operations[operation] == "" || len(hosts) == 0 {
			return false
		}
		seenHosts := make(map[string]bool)
		for _, host := range hosts {
			if _, ok := p.Hosts[host]; !ok || seenHosts[host] {
				return false
			}
			seenHosts[host] = true
		}
	}
	for repo, entry := range p.DirectPRVerification {
		if !fleet.ValidRepo(repo) || !pcName.MatchString(entry.Requester) || entry.Requester == p.Signer.Token || !p.allows(entry.Host, "verify") {
			return false
		}
	}
	seen := make(map[string]bool)
	for _, operation := range p.ReadOnlyOperations {
		if !pcName.MatchString(operation) || seen[operation] || p.Operations[operation] == "" {
			return false
		}
		seen[operation] = true
	}
	return true
}
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
func grantKind(kind string) bool {
	return contains([]string{"nas-claude-rule", "hostd-repair", "host-ssh", "implementation-limit-retry", "db-read"}, kind)
}
func (s *service) targetAllowed(kind, target string) bool {
	switch kind {
	case "db-read":
		return contains([]string{"shared", "app"}, target)
	case "implementation-limit-retry":
		return regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*$`).MatchString(target)
	case "nas-claude-rule":
		return contains([]string{"usage-per-issue", "docker-inspect", "docker-stats", "docker-compose-ps"}, target)
	case "hostd-repair", "host-ssh":
		parts := strings.Split(target, ":")
		if len(parts) != 2 || !pcName.MatchString(parts[0]) {
			return false
		}
		if kind == "hostd-repair" {
			return contains([]string{"restore-hostd-files", "set-node-path", "quarantine-old-requests", "send-log-tail", "retry-failed-image"}, parts[1])
		}
		p, e := s.repairPolicy()
		if e != nil {
			return false
		}
		return p.allows(parts[0], parts[1])
	}
	return false
}
