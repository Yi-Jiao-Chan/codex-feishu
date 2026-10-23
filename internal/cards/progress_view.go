package cards

// MaxProgressRecords leaves room for task controls within the card element limit.
const MaxProgressRecords = 32

// MaxProgressToolItems bounds retained per-call metadata within each group.
// The renderer also reserves the platform's component and JSON byte budgets.
const MaxProgressToolItems = 32

// ProgressTool is one native invocation, not a deduplicated tool category.
// Details contain bounded display metadata, never commands, arguments or output.
type ProgressTool struct {
	ItemID, Kind, Status, Title string
	Details                     []string
}

// ProgressTools summarizes adjacent native tool items, never their arguments or
// output. Declined and unknown items are separate from confirmed execution.
type ProgressTools struct {
	Running, Completed, Failed, Declined, Unknown int
	Details                                       []string
	Items                                         []ProgressTool
	OmittedItems                                  int
}

// ProgressRecord is either a public message or a group of adjacent tools.
// Text timestamps first local reception, not native generation. Tool groups do
// not invent timestamps. ReceivedAtMS == 0 means no reception time was recorded.
type ProgressRecord struct {
	ItemID       string
	Text         string
	ReceivedAtMS int64
	Tools        *ProgressTools
}

// ProgressView contains public per-turn presentation facts. Records retain
// native item order; they never contain user input, final replies or reasoning.
// Non-positive elapsed time is omitted rather than inventing a duration.
type ProgressView struct {
	Body           string
	CurrentItemID  string
	Records        []ProgressRecord
	TimeZone       string
	OmittedRecords int
	ElapsedSeconds int64
	RunningTools   int
	FinishedTools  int
}
