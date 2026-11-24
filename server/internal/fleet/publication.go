package fleet

// PublicationPhase identifies the publication phases reported by workers.
func PublicationPhase(view map[string]any) bool {
	phase := Text(view["phase"])
	return phase == "publishing" || phase == "releasing" || phase == "deploying" || phase == "" && (view["publishPending"] == true || view["releasePending"] == true)
}
