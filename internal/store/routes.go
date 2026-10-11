package store

import "context"

// Bindings and BindingByChat are deployment-scoped: a chat supplied by an
// incoming event never chooses a different host's native thread.
func (s *Store) Bindings(ctx context.Context, host string) ([]Binding, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT thread_id,intent_id,chat_id FROM bindings WHERE host_id=? ORDER BY thread_id", host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Binding
	for rows.Next() {
		b := Binding{HostID: host}
		if err := rows.Scan(&b.ThreadID, &b.IntentID, &b.ChatID); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

func (s *Store) BindingByChat(ctx context.Context, host, chat string) (Binding, error) {
	b := Binding{HostID: host, ChatID: chat}
	err := s.DB.QueryRowContext(ctx, "SELECT thread_id,intent_id FROM bindings WHERE host_id=? AND chat_id=?", host, chat).Scan(&b.ThreadID, &b.IntentID)
	return b, err
}
