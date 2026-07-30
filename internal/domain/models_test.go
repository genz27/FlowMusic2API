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
		if model.SelectedModel == "" {
			t.Fatalf("model %s missing FlowMusic selected_model", model.ID)
		}
	}
}

func TestResolveGenerationModelLyria35Aliases(t *testing.T) {
	tests := map[string]string{
		"lyria":       "lyria",
		"lyria-3.5":   "lyria",
		"lyria-pro":   "lyria-pro",
		"lyria-3-pro": "lyria-pro",
	}
	for input, wantID := range tests {
		got, ok := ResolveGenerationModel(input)
		if !ok || got.ID != wantID {
			t.Fatalf("ResolveGenerationModel(%q) = %+v ok=%v, want id %q", input, got, ok, wantID)
		}
	}
	standard, _ := ResolveGenerationModel("lyria")
	if standard.SelectedModel != "Lyria 3.5" {
		t.Fatalf("lyria selected_model = %q, want Lyria 3.5", standard.SelectedModel)
	}
	pro, _ := ResolveGenerationModel("lyria-pro")
	if pro.SelectedModel != "Lyria 3 Pro" {
		t.Fatalf("lyria-pro selected_model = %q, want Lyria 3 Pro", pro.SelectedModel)
	}
}
