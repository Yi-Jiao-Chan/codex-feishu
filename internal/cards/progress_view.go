package cards

// MaxProgressRecords leaves room for task controls within the card element limit.
const MaxProgressRecords = 32

// ProgressRecord timestamps first local reception, not native generation.
// ReceivedAtMS == 0 means that no reliable reception time was recorded.
type ProgressRecord struct {
	ItemID       string
	Text         string
	ReceivedAtMS int64
}

// ProgressView contains public per-turn presentation facts. Records retain
// native item order; they never contain user input, final replies or reasoning.
// Non-positive elapsed time is omitted rather than inventing a duration.
type ProgressView struct {
	Body           string
	Records        []ProgressRecord
	TimeZone       string
	OmittedRecords int
	ElapsedSeconds int64
	RunningTools   int
	FinishedTools  int
}
