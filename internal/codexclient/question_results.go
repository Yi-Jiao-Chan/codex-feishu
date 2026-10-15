package codexclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// The typed history API does not materialize native question outputs in
// 0.162.1. This read-only adapter uses the daemon-provided rollout path and
// validates the session, turn, call and original questions before returning
// an actual tool result. It never infers the winning client from resolved.
var ErrQuestionResultUnavailable = errors.New("native question result unavailable")

const questionRolloutMaxBytes = 64 << 20
const questionRolloutMaxLine = 4 << 20

type QuestionResultReader struct {
	Metadata func(context.Context, string) (Thread, error)
	Roots    []string
	mu       sync.Mutex
	paths    map[string]string
	checked  map[string]time.Time
	cache    map[string]questionResultCache
}

type questionResultCache struct {
	size, modified int64
	device, inode  uint64
	result         json.RawMessage
}

func NewQuestionResultReader(metadata func(context.Context, string) (Thread, error)) *QuestionResultReader {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".codex")
		}
	}
	r := &QuestionResultReader{Metadata: metadata}
	if filepath.IsAbs(home) {
		r.Roots = []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")}
	}
	return r
}

func (r *QuestionResultReader) Read(ctx context.Context, request InteractiveRequest) (json.RawMessage, error) {
	if request.Kind != "question" || request.ThreadID == "" || request.TurnID == "" || request.ItemID == "" {
		return nil, ErrQuestionResultUnavailable
	}
	for _, q := range request.Questions {
		if q.Secret {
			return nil, ErrQuestionResultUnavailable
		}
	}
	r.mu.Lock()
	path := r.paths[request.ThreadID]
	if time.Since(r.checked[request.ThreadID]) > 5*time.Second {
		path = ""
	}
	r.mu.Unlock()
	if path == "" {
		if r.Metadata == nil {
			return nil, ErrQuestionResultUnavailable
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		thread, err := r.Metadata(call, request.ThreadID)
		cancel()
		if err != nil || thread.ID != request.ThreadID || thread.Path == nil {
			return nil, ErrQuestionResultUnavailable
		}
		path = *thread.Path
		r.mu.Lock()
		if len(r.paths) >= 256 {
			r.paths = nil
			r.checked = nil
		}
		if r.paths == nil {
			r.paths = map[string]string{}
			r.checked = map[string]time.Time{}
		}
		r.paths[request.ThreadID] = path
		r.checked[request.ThreadID] = time.Now()
		r.mu.Unlock()
	}
	f, err := r.open(path)
	if err != nil {
		r.mu.Lock()
		delete(r.paths, request.ThreadID) // Archive can move a rollout; refresh metadata next tick.
		r.mu.Unlock()
		return nil, ErrQuestionResultUnavailable
	}
	defer f.Close()
	var before, after unix.Stat_t
	if unix.Fstat(int(f.Fd()), &before) != nil || before.Size > questionRolloutMaxBytes {
		return nil, ErrQuestionResultUnavailable
	}
	signature := questionResultCache{size: before.Size, modified: before.Mtim.Nano(), device: uint64(before.Dev), inode: before.Ino}
	questions, _ := json.Marshal(request.Questions)
	key := request.ThreadID + "\x00" + request.TurnID + "\x00" + request.ItemID + "\x00" + string(questions)
	r.mu.Lock()
	prior, exists := r.cache[key]
	r.mu.Unlock()
	if exists && prior.size == signature.size && prior.modified == signature.modified && prior.device == signature.device && prior.inode == signature.inode {
		if len(prior.result) == 0 {
			return nil, ErrQuestionResultUnavailable
		}
		return bytes.Clone(prior.result), nil
	}
	result, err := readQuestionRollout(ctx, f, request)
	if unix.Fstat(int(f.Fd()), &after) != nil || before.Size != after.Size || before.Mtim != after.Mtim || ctx.Err() != nil {
		return nil, ErrQuestionResultUnavailable
	}
	if err == nil {
		signature.result = bytes.Clone(result)
	}
	r.mu.Lock()
	if len(r.cache) >= 256 {
		r.cache = nil
	}
	if r.cache == nil {
		r.cache = map[string]questionResultCache{}
	}
	r.cache[key] = signature
	r.mu.Unlock()
	return result, err
}

// Pin each directory component; a daemon path cannot redirect this reader to
// arbitrary files via traversal or symlinks. Do not change Codex file modes.
func (r *QuestionResultReader) open(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrQuestionResultUnavailable
	}
	for _, configuredRoot := range r.Roots {
		root, err := filepath.EvalSymlinks(configuredRoot)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			// Accept a trusted home alias only by replacing its root prefix.
			rel, err = filepath.Rel(filepath.Clean(configuredRoot), path)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				continue
			}
		}
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		ok := true
		for i, part := range parts {
			var stat unix.Stat_t
			if unix.Fstat(fd, &stat) != nil || int(stat.Uid) != os.Geteuid() || stat.Mode&0022 != 0 {
				ok = false
				break
			}
			flags := unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
			if i < len(parts)-1 {
				flags |= unix.O_DIRECTORY
			}
			next, openErr := unix.Openat(fd, part, flags, 0)
			unix.Close(fd)
			fd = next
			if openErr != nil {
				ok = false
				break
			}
		}
		var stat unix.Stat_t
		if ok && unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG && int(stat.Uid) == os.Geteuid() && stat.Mode&0022 == 0 {
			return os.NewFile(uintptr(fd), "native-question-result"), nil
		}
		if fd >= 0 {
			unix.Close(fd)
		}
	}
	return nil, ErrQuestionResultUnavailable
}

func readQuestionRollout(ctx context.Context, file io.Reader, request InteractiveRequest) (json.RawMessage, error) {
	scan := bufio.NewScanner(io.LimitReader(file, questionRolloutMaxBytes+1))
	scan.Buffer(make([]byte, 64<<10), questionRolloutMaxLine)
	meta, starts, calls, outputs := 0, 0, 0, 0
	currentTurn := ""
	var result json.RawMessage
	for scan.Scan() {
		if ctx.Err() != nil || !receiptJSONUnique(scan.Bytes()) {
			return nil, ErrQuestionResultUnavailable
		}
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			return nil, ErrQuestionResultUnavailable
		}
		var p struct {
			Type        string          `json:"type"`
			ID          string          `json:"id"`
			TurnID      string          `json:"turn_id"`
			Name        string          `json:"name"`
			Namespace   *string         `json:"namespace"`
			CallID      string          `json:"call_id"`
			Arguments   json.RawMessage `json:"arguments"`
			Output      json.RawMessage `json:"output"`
			HistoryBase json.RawMessage `json:"history_base"`
		}
		if json.Unmarshal(record.Payload, &p) != nil || len(p.HistoryBase) > 0 && string(p.HistoryBase) != "null" || p.Type == "history_base" {
			return nil, ErrQuestionResultUnavailable
		}
		if record.Type != "session_meta" && meta != 1 {
			return nil, ErrQuestionResultUnavailable
		}
		if record.Type == "compacted" || p.Type == "thread_rolled_back" {
			if starts > 0 {
				return nil, ErrQuestionResultUnavailable
			}
			// Earlier history rewrites cannot provide the target scope.
			// A later explicit task_started must establish it afresh.
			currentTurn = ""
			continue
		}
		switch record.Type {
		case "session_meta":
			meta++
			if meta != 1 || p.ID != request.ThreadID {
				return nil, ErrQuestionResultUnavailable
			}
		case "event_msg":
			switch p.Type {
			case "task_started", "turn_started":
				currentTurn = p.TurnID
				if currentTurn == request.TurnID {
					starts++
				}
			case "task_complete", "turn_complete", "turn_aborted":
				// Abort may omit turn_id; an ambiguous terminal cannot leave
				// this scope open for a late output from another turn.
				currentTurn = ""
			}
		case "response_item":
			if p.CallID != request.ItemID {
				continue
			}
			switch p.Type {
			case "function_call":
				calls++
				var arguments string
				if currentTurn != request.TurnID || starts != 1 || calls != 1 || p.Name != "request_user_input" || p.Namespace != nil && *p.Namespace != "" && *p.Namespace != "functions" || json.Unmarshal(p.Arguments, &arguments) != nil || !questionArgumentsMatch(arguments, request.Questions) {
					return nil, ErrQuestionResultUnavailable
				}
			case "function_call_output":
				outputs++
				if currentTurn != request.TurnID || calls != 1 || outputs != 1 {
					return nil, ErrQuestionResultUnavailable
				}
				var output string
				if json.Unmarshal(p.Output, &output) != nil {
					return nil, ErrQuestionResultUnavailable
				}
				answers, err := ParseQuestionAnswers(request, []byte(output))
				if err != nil {
					return nil, err
				}
				result, _ = json.Marshal(map[string]any{"answers": answers})
			}
		}
	}
	if scan.Err() != nil || meta != 1 || starts != 1 || calls != 1 || outputs != 1 || len(result) == 0 {
		return nil, ErrQuestionResultUnavailable
	}
	return result, nil
}

func questionArgumentsMatch(arguments string, expected []InputQuestion) bool {
	if !receiptJSONUnique([]byte(arguments)) {
		return false
	}
	var args struct {
		Questions []InputQuestion `json:"questions"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil || len(args.Questions) != len(expected) {
		return false
	}
	for i := range args.Questions {
		args.Questions[i].Other = true // Native tool normalization, 0.162.1.
		if len(args.Questions[i].Options) == 0 {
			args.Questions[i].Options = nil
		}
		q := expected[i]
		if len(q.Options) == 0 {
			q.Options = nil
		}
		if q.Secret || !reflect.DeepEqual(args.Questions[i], q) {
			return false
		}
	}
	return true
}

type QuestionAnswer struct {
	Answers []string `json:"answers"`
}

// ParseQuestionAnswers validates recorded or locally saved answer evidence.
// App free text may exceed the Feishu input limit; do not rewrite or truncate it.
func ParseQuestionAnswers(request InteractiveRequest, result []byte) (map[string]QuestionAnswer, error) {
	if request.Kind != "question" || len(result) > 1<<20 || !receiptJSONUnique(result) {
		return nil, ErrQuestionResultUnavailable
	}
	var value struct {
		Answers map[string]QuestionAnswer `json:"answers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || len(value.Answers) != len(request.Questions) || len(value.Answers) == 0 {
		return nil, ErrQuestionResultUnavailable
	}
	for _, q := range request.Questions {
		answer, ok := value.Answers[q.ID]
		if q.Secret || !ok || len(answer.Answers) == 0 || len(answer.Answers) > 64 {
			return nil, ErrQuestionResultUnavailable
		}
		for _, v := range answer.Answers {
			if strings.TrimSpace(v) == "" || !utf8.ValidString(v) {
				return nil, ErrQuestionResultUnavailable
			}
		}
	}
	return value.Answers, nil
}

// Reject duplicate JSON keys rather than silently selecting the last answer,
// call identity or turn boundary. Keep recursion bounded for local records.
func receiptJSONUnique(value []byte) bool {
	if !utf8.Valid(value) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	var read func(int) bool
	read = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
			}
			if !read(depth + 1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && (delimiter == '{' && end == json.Delim('}') || delimiter == '[' && end == json.Delim(']'))
	}
	if !read(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
