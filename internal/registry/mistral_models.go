package registry

// staticMistralModels lists the models a Mistral Vibe sign-in key serves. The IDs come
// from the Mistral Vibe CLI defaults. The list is hard-coded, not part of models.json,
// because the remote catalog refresh replaces models.json wholesale and has no
// Mistral section.
var staticMistralModels = []*ModelInfo{
	{
		ID:                       "mistral-large-4-0",
		Object:                   "model",
		Type:                     "mistral",
		OwnedBy:                  "mistralai",
		DisplayName:              "Mistral Large 4",
		ContextLength:            1048576,
		SupportedInputModalities: []string{"text", "image"},
	},
	{
		ID:                       "mistral-vibe-cli-latest",
		Object:                   "model",
		Type:                     "mistral",
		OwnedBy:                  "mistralai",
		DisplayName:              "Mistral Medium 3.5 (Vibe CLI)",
		ContextLength:            262144,
		SupportedInputModalities: []string{"text", "image"},
	},
	{
		ID:          "mistral-vibe-cli-fast",
		Object:      "model",
		Type:        "mistral",
		OwnedBy:     "mistralai",
		DisplayName: "Mistral Small (Vibe CLI fast)",
	},
	{
		ID:          "mistral-small-latest",
		Object:      "model",
		Type:        "mistral",
		OwnedBy:     "mistralai",
		DisplayName: "Mistral Small",
	},
}

// GetMistralModels returns the built-in Mistral model definitions.
func GetMistralModels() []*ModelInfo {
	return cloneModelInfos(staticMistralModels)
}
