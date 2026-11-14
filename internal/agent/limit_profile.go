package agent

const (
	defaultLimitProfile   = "default"
	localQwenLimitProfile = "wis702-local-qwen35-9b"
	localQwenModel        = "unsloth/Qwen3.5-9B-GGUF:Q4_K_M"
	localQwenRevision     = "sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8"
)

// The WIS-702 experiment is restricted to the measured 16,384-context local model pin.
func localQwenProfileEligible(r Run) bool {
	return r.RequestKind == RequestCase && r.ProviderName == ProviderLocal && r.ProviderActual &&
		r.Model == localQwenModel && r.ModelRevision == localQwenRevision
}

func limitsForRun(r Run, l Limits, p RequestPlan) (Limits, error) {
	if r.LimitProfileVersion != 1 {
		return Limits{}, ErrInvalidBudget
	}
	switch r.LimitProfileID {
	case defaultLimitProfile:
		return l, nil
	case localQwenLimitProfile:
		if localQwenProfileEligible(r) {
			// The byte estimate can undercount; only a measured request gets the experiment.
			l.MaxInputTokens = min(l.MaxInputTokens, DefaultLimits().MaxInputTokens)
			if p.InputTokens > 0 && p.LocalContextTokens >= 12000+int64(l.MaxOutputTokens) {
				l.MaxInputTokens = 12000
			}
			return l, nil
		}
	}
	return Limits{}, ErrInvalidBudget
}
