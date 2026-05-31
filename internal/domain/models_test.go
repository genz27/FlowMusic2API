package domain

import "testing"

func TestGenerationModelsUseAllowedFlowMusicModes(t *testing.T) {
	allowed := map[string]bool{
		"onboarding": true,
		"standard":   true,
		"tiny":       true,
		"mini":       true,
	}
	for _, model := range GenerationModels() {
		if !allowed[model.FlowMusicMode] {
			t.Fatalf("model %s uses unsupported FlowMusic mode %q", model.ID, model.FlowMusicMode)
		}
	}
}
