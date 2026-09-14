package loan

import "time"

type PositionSource string

const (
	SourceMSO           PositionSource = "MSO"
	SourceDWH           PositionSource = "DWH"
	SourceTodaySnapshot PositionSource = "current_snapshot"
	SourceReconstructed PositionSource = "reconstructed"
)

type LoanPosition struct {
	AsOf                 Date
	AccountNumber        string
	PrincipalOutstanding Money
	PrincipalDue         Money
	InterestDue          Money
	CollectabilityBI     int
	UnappliedAmount      Money
	Source               PositionSource
	Branch               string
	Product              string
	CIF                  string
	ContractNumber       string
	SourceUpdatedAt      *time.Time
}

type OpeningLoanState struct {
	AccountNumber        string
	InterestType         string
	PrincipalOutstanding Money
	PrincipalDue         Money
	InterestDue          Money
	CollectabilityBI     int
}

type CollectabilityPoint struct {
	Date  Date
	Value int
}

type Repayment struct {
	Date                  Date
	PrincipalComponent    Money
	InterestComponent     Money
	PenaltyComponent      Money
	EarlyPenaltyComponent Money
	DWPComponent          Money
	TotalPayment          Money
	JournalNumber         string
	SourceOrder           int
}

type ContractualInstallment struct {
	Number  int
	DueDate Date
}

type ContractualInstallmentEvidence struct {
	Number     int64
	RawDueDate string
}

type ContractualScheduleRow struct {
	Number           int
	DueDate          Date
	Principal        Money
	Interest         Money
	Installment      Money
	ScheduledBalance Money
}

type ContractData struct {
	PrimaryAccount           string
	AlternateAccount         string
	CIF                      string
	CustomerName             string
	Branch                   string
	Product                  string
	PlafondLimit             Money
	TenorMonths              int
	FlatRatePercent          Money
	ReferenceRatePercent     Money
	CurrentCollectability    int
	CurrentPrincipalDue      Money
	CurrentInterestDue       Money
	PenaltyDue               Money
	Status                   string
	CloseDate                Date
	ContractChanged          bool
	RawScheduleCount         int
	ContractScheduleEvidence []ContractualInstallmentEvidence
	ContractSchedule         []ContractualInstallment
	Repayments               []Repayment
}

type CalculationInput struct {
	AsOf                   Date
	Cutoff                 Date
	ContractualPrincipal   Money
	TenorMonths            int
	FlatRatePercent        Money
	Opening                OpeningLoanState
	ContractSchedule       []ContractualInstallment
	Repayments             []Repayment
	CollectabilityTimeline []CollectabilityPoint
}

type CalculationTrace struct {
	Opening            OpeningLoanState
	PeriodsAccrued     int
	RepaymentsApplied  int
	LastPaymentDate    Date
	LastDueDateAccrued Date
}

type CalculationResult struct {
	AsOf                 Date
	PrincipalOutstanding Money
	PrincipalDue         Money
	InterestDue          Money
	CollectabilityBI     int
	UnappliedAmount      Money
	Trace                CalculationTrace
	ContractualSchedule  []ContractualScheduleRow
}

type ResolvedPosition struct {
	Loan                ContractData
	Position            LoanPosition
	Trace               CalculationTrace
	ContractualSchedule []ContractualScheduleRow
}
