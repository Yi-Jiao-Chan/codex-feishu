package cards

// ProgressView contains public per-turn presentation facts. Records retain
// native item order; they never contain user input, final replies or reasoning.
// Non-positive elapsed time is omitted rather than inventing a duration.
type ProgressView struct {
	Body           string
	Records        []string
	OmittedRecords int
	ElapsedSeconds int64
	RunningTools   int
	FinishedTools  int
}
