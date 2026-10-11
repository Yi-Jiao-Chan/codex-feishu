package devpackage

import "errors"

// Failure is a fixed, non-sensitive machine-readable failure state. Its
// message intentionally contains no subprocess output, source path, or input
// content.
type Failure struct {
	State string
}

func (f Failure) Error() string { return f.State }

func failure(state string) error { return Failure{State: state} }

func preserveFailure(state string, err error) error {
	var known Failure
	if errors.As(err, &known) {
		return known
	}
	return failure(state)
}

// FailureState converts any package error to the safe state emitted by the
// CLI. Unknown implementation failures remain deliberately generic.
func FailureState(err error) string {
	if err == nil {
		return ""
	}
	var known Failure
	if errors.As(err, &known) && known.State != "" {
		return known.State
	}
	return "error"
}
