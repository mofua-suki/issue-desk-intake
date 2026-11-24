package contract

import "testing"

func TestMeaningfulApprovalRefusesShortAndBoilerplate(t *testing.T) {
	for _, s := range []string{"", "承認します", "  よろしくお願いいたします。 ", "どうぞよろしくお願いいたします！", "Approved!!!!!", "お願いいたします……"} {
		if MeaningfulApproval(s) {
			t.Errorf("%q を承認の発言と扱った", s)
		}
	}
	for _, s := range []string{"共通番号12の相談の続行を承認します", "1-A、代理回答の取消を進めて"} {
		if !MeaningfulApproval(s) {
			t.Errorf("%q を断った", s)
		}
	}
}
