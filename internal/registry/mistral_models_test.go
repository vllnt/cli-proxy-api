package registry

import "testing"

func TestGetMistralModels(t *testing.T) {
	models := GetMistralModels()
	if len(models) == 0 {
		t.Fatal("expected built-in Mistral models")
	}
	seen := map[string]bool{}
	for _, model := range models {
		if model == nil || model.ID == "" {
			t.Fatalf("invalid model entry: %+v", model)
		}
		if seen[model.ID] {
			t.Fatalf("duplicate model id %q", model.ID)
		}
		seen[model.ID] = true
		if model.Type != "mistral" || model.OwnedBy != "mistralai" {
			t.Fatalf("model %q has type %q owned_by %q, want mistral/mistralai", model.ID, model.Type, model.OwnedBy)
		}
		if model.Thinking != nil {
			t.Fatalf("model %q must not declare thinking support: no Mistral thinking applier exists", model.ID)
		}
	}
	for _, id := range []string{"mistral-large-4-0", "mistral-vibe-cli-latest", "mistral-vibe-cli-fast", "mistral-small-latest"} {
		if !seen[id] {
			t.Fatalf("missing model %q", id)
		}
	}
}

func TestGetMistralModelsReturnsClones(t *testing.T) {
	first := GetMistralModels()
	first[0].DisplayName = "mutated"
	first[0].SupportedInputModalities[0] = "mutated"
	if got := GetMistralModels()[0]; got.DisplayName == "mutated" || got.SupportedInputModalities[0] == "mutated" {
		t.Fatalf("caller mutation leaked into the static list: %+v", got)
	}
}

func TestMistralModelsAreAvailableByChannelAndLookup(t *testing.T) {
	byChannel := GetStaticModelDefinitionsByChannel(" Mistral ")
	if len(byChannel) != len(GetMistralModels()) {
		t.Fatalf("channel lookup returned %d models, want %d", len(byChannel), len(GetMistralModels()))
	}
	if info := LookupStaticModelInfoByChannel("mistral-large-4-0", "mistral"); info == nil || info.ContextLength != 1048576 {
		t.Fatalf("channel model lookup = %+v", info)
	}
	if info := LookupStaticModelInfo("mistral-vibe-cli-latest"); info == nil || info.Type != "mistral" {
		t.Fatalf("global model lookup = %+v", info)
	}
	if info := LookupStaticModelInfoByChannel("mistral-large-4-0", "meta"); info != nil {
		t.Fatalf("Mistral model must not resolve under another provider: %+v", info)
	}
}
