//go:build integration

package agent

func SetChatToolHooks(t *Tools, beforeRead, afterRead func()) {
	t.beforeRead, t.afterRead = beforeRead, afterRead
}

func SetBeforeChatAnswerFinishTx(r *Runtime, f func()) {
	r.beforeChatAnswerFinishTx = f
}

// SetAfterSiteAdmission runs f after a new case run passes the site count and before it is
// inserted, so a test can line two admissions up on the same site.
func SetAfterSiteAdmission(r *Runtime, f func()) {
	r.afterSiteAdmission = f
}
