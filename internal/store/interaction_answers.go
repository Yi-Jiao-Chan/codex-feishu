package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Native answer evidence is separate from Feishu submission attempts. Closure
// and a saved local answer cannot establish an effective answer by themselves.
const InteractionAnswersSchema = `
CREATE TABLE IF NOT EXISTS interaction_answers (
 request_key TEXT PRIMARY KEY REFERENCES interaction_requests(request_key),
 result_json TEXT NOT NULL
);
`

func (s *Store) InteractionAnswer(ctx context.Context, key string) (string, error) {
	var result string
	err := s.DB.QueryRowContext(ctx, `SELECT result_json FROM interaction_answers WHERE request_key=?`, key).Scan(&result)
	return result, err
}

func (s *Store) RecordInteractionAnswer(ctx context.Context, key, result string) error {
	if !json.Valid([]byte(result)) || result == "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request, err := scanInteraction(tx.QueryRowContext(ctx, `SELECT `+interactionColumns+` FROM interaction_requests WHERE request_key=?`, key))
	if err != nil {
		return err
	}
	question := request.Mode == "native" && request.Method == "item/tool/requestUserInput" || request.Mode == "async" && request.Method == "async"
	if request.State != "resolved" || !question {
		return ErrConflict
	}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT result_json FROM interaction_answers WHERE request_key=?`, key).Scan(&prior)
	if err == nil {
		if prior != result {
			return ErrConflict
		}
	} else if err == sql.ErrNoRows {
		if _, err = tx.ExecContext(ctx, `INSERT INTO interaction_answers(request_key,result_json) VALUES(?,?)`, key, result); err != nil {
			return err
		}
	} else {
		return err
	}
	return tx.Commit()
}
