package httpapi

import "testing"

func TestEvidenceJSONNeverSilentlyReplacesMalformedUnicode(t *testing.T) {
	for _, body := range []string{`{"text":"machine λ"}`, `{"text":"\ud83d\ude00"}`, `{"text":"\\ud800"}`, `{"text":"replacement � was actually supplied"}`, `{"text":"\u00e9"}`} {
		if !validJSONUnicode([]byte(body)) {
			t.Errorf("valid Unicode rejected: %s", body)
		}
	}
	for _, body := range []string{"{\"text\":\"\xff\"}", `{"text":"\ud800"}`, `{"text":"\udc00"}`, `{"text":"\ud800\u0041"}`, `{"text":"\ud800x\udc00"}`} {
		if validJSONUnicode([]byte(body)) {
			t.Errorf("malformed Unicode accepted: %s", body)
		}
	}
}
