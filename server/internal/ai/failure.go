package ai

// Failure carries a fixed, public explanation. Provider output is used only to
// select a class and is never copied into persisted errors or API responses.
type Failure struct {
	Field     string
	DeskCode  string
	Kind      string
	Message   string
	Temporary bool
}

func (e *Failure) Error() string         { return e.Message }
func (e *Failure) PublicMessage() string { return e.Message }
func (e *Failure) Transient() bool       { return e.Temporary }
func (e *Failure) Timeout() bool         { return e.Kind == "timeout" || e.Kind == "stalled" }
func (e *Failure) Code() string          { return e.DeskCode }
func failed(kind, message string, temporary bool) *Failure {
	return &Failure{Kind: kind, Message: message, Temporary: temporary}
}
