package hostd

import (
	"fmt"
	"sort"
	"strings"
)

// overrideWarning は、共通（*）の行を登録したのに、PC専用の行が残っていて、その PC の望む版が変わらない場合の警告。
// Desired は PC専用の行を共通の行より優先する（service.go の ORDER BY (host=$1) DESC）。
func overrideWarning(flavor string, hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)
	name := "client"
	if flavor != "" {
		name = "client" + flavor
	}
	return fmt.Sprintf("共通（*）の行を登録しましたが、PC専用の行が別の版で残っているため、次のPCの望む版は変わりません（%s の%s）。PC専用の行が共通の行より優先されます。これらのPCにも反映するには、PCごとに host を指定して登録してください。", strings.Join(sorted, "・"), name)
}

// registryChangeWarning は、PC専用の行のレジストリ名が、その PC の前回の登録と違う場合の警告。
// 各 PC の hostd は、起動中の compose の image が「登録のレジストリ/issue-desk-client<flavor>:タグ」と一致しないと更新を戻す（digest_mismatch）。
func registryChangeWarning(host, previous, next string) string {
	if previous == "" || previous == next {
		return ""
	}
	return fmt.Sprintf("%s の登録のレジストリ名が前回の登録（%s）と違います（今回: %s）。各PCの .env の REGISTRY と一致しないと、hostd の照合が合わず更新を戻します（失敗した版は同じタグで再試行できません）。意図した変更でなければ、正しい値で登録し直してください。", host, previous, next)
}
