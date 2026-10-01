package zkcircuit

import "fmt"

// Failure kinds reported by the persistent workbench. Callers can inspect a
// returned error with errors.Is to distinguish rule violations; human-facing
// clients may also use the messages directly.
var (
	// ErrInvalidArgument is a malformed request (empty name, bad counts, …).
	ErrInvalidArgument = StoreError{Kind: "invalid argument"}
	// ErrNotFound means the referenced circuit version does not exist.
	ErrNotFound = StoreError{Kind: "not found"}
	// ErrConflict means an existing record disagrees with the request.
	ErrConflict = StoreError{Kind: "conflict"}
	// ErrDraftRequired means an operation requires a draft circuit version.
	ErrDraftRequired = StoreError{Kind: "draft required"}
	// ErrFrozen means the circuit version is frozen and cannot be modified.
	ErrFrozen = StoreError{Kind: "frozen"}
	// ErrNotFrozen means trusted setup or a job requires a frozen version.
	ErrNotFrozen = StoreError{Kind: "not frozen"}
	// ErrSetupMissing means the frozen version has no recorded trusted setup.
	ErrSetupMissing = StoreError{Kind: "trusted setup missing"}
	// ErrUnsupportedKind means a job kind other than prove was submitted.
	ErrUnsupportedKind = StoreError{Kind: "unsupported job kind"}
	// ErrDefinitionMissing means a frozen version has registered counts but no
	// imported constraint definition, so it cannot be compiled.
	ErrDefinitionMissing = StoreError{Kind: "constraint definition missing"}
	// ErrArtifactMissing means the version has no compiled artifact.
	ErrArtifactMissing = StoreError{Kind: "compiled artifact missing"}
	// ErrArtifactMismatch means the supplied hash does not belong to the
	// target version's compiled artifact.
	ErrArtifactMismatch = StoreError{Kind: "artifact mismatch"}
	// ErrInvalidInput means a witness document is malformed or does not match
	// the version's declared input layout.
	ErrInvalidInput = StoreError{Kind: "input format error"}
	// ErrDataCorrupt means the on-disk data file cannot be read safely.
	ErrDataCorrupt = StoreError{Kind: "data corrupt"}
)

// StoreError is a domain error with a human-readable Detail tagged with a
// stable Kind. The zero value is not used; construct via the helpers below.
type StoreError struct {
	Kind   string
	Detail string
}

func (e StoreError) Error() string {
	if e.Detail == "" {
		return e.Kind
	}
	return e.Kind + ": " + e.Detail
}

// Is supports errors.Is on Kind while ignoring Detail.
func (e StoreError) Is(target error) bool {
	other, ok := target.(StoreError)
	return ok && other.Kind == e.Kind
}

func invalidf(format string, args ...any) error {
	return StoreError{Kind: ErrInvalidArgument.Kind, Detail: fmt.Sprintf(format, args...)}
}

func notFoundf(format string, args ...any) error {
	return StoreError{Kind: ErrNotFound.Kind, Detail: fmt.Sprintf(format, args...)}
}

func conflictf(format string, args ...any) error {
	return StoreError{Kind: ErrConflict.Kind, Detail: fmt.Sprintf(format, args...)}
}

func draftRequiredf(format string, args ...any) error {
	return StoreError{Kind: ErrDraftRequired.Kind, Detail: fmt.Sprintf(format, args...)}
}

func frozenf(format string, args ...any) error {
	return StoreError{Kind: ErrFrozen.Kind, Detail: fmt.Sprintf(format, args...)}
}

func notFrozenf(format string, args ...any) error {
	return StoreError{Kind: ErrNotFrozen.Kind, Detail: fmt.Sprintf(format, args...)}
}

func setupMissingf(format string, args ...any) error {
	return StoreError{Kind: ErrSetupMissing.Kind, Detail: fmt.Sprintf(format, args...)}
}

func corruptf(format string, args ...any) error {
	return StoreError{Kind: ErrDataCorrupt.Kind, Detail: fmt.Sprintf(format, args...)}
}

func inputFormatf(format string, args ...any) error {
	return StoreError{Kind: ErrInvalidInput.Kind, Detail: fmt.Sprintf(format, args...)}
}
