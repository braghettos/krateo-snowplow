package v1

// ResourceRefTemplateItem defines a single resource reference template.
type ResourceRefTemplate struct {
	// Iterator defines a field on which iterate.
	Iterator *string `json:"iterator,omitempty"`
	// Template defines the template for a resource reference.
	Template ResourceRef `json:"template,omitempty"`
}
