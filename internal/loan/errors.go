package loan

import "errors"

var (
	ErrNotFound                   = errors.New("loan not found")
	ErrAmbiguousAccountResolution = errors.New("loan account resolution is ambiguous")
	ErrInvalidInput               = errors.New("invalid input")
	ErrUnsupportedCalculation     = errors.New("unsupported calculation")
	ErrHistoricalEvidence         = errors.New("historical evidence unavailable")
	ErrCurrentSnapshot            = errors.New("current snapshot unavailable")
	ErrFincloudCredentials        = errors.New("Fincloud credentials rejected")
	ErrFincloudSession            = errors.New("Fincloud session rejected")
	ErrFincloudUnavailable        = errors.New("Fincloud upstream unavailable")
	ErrDWHUnavailable             = errors.New("DWH unavailable")
	ErrMSOUnavailable             = errors.New("MSO unavailable")
	ErrInvariant                  = errors.New("calculation invariant violation")
)
